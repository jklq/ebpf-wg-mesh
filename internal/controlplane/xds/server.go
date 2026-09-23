package xds

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	xdslog "github.com/envoyproxy/go-control-plane/pkg/log"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
)

type NodeStatus struct {
	Applied  map[string]string
	NACKs    int64
	LastNACK string
}

// FullyApplied reports whether this node ACKed version across every required
// type. A node mid-apply, holding an older version, or holding a rejection
// for any type is not fully applied.
func (n NodeStatus) FullyApplied(version string, required []string) bool {
	for _, typeURL := range required {
		if n.Applied[typeURL] != version {
			return false
		}
	}
	return true
}

type Status struct {
	Version       string
	Counts        Counts
	PublishedAt   time.Time
	HasSnapshot   bool
	RequiredTypes []string
	Nodes         map[string]NodeStatus
}

// Server is the xDS management server. It serves the latest published
// snapshot over ADS (plus per-type SotW), tracks per-node ACK/NACK, and
// retains the last-known-good snapshot across NACKs and reconnects.
type Server struct {
	cache cachev3.SnapshotCache
	xds   serverv3.Server

	mu         sync.Mutex
	current    *Snapshot
	published  time.Time
	streams    map[int64]string
	streamCtxs map[int64]context.Context
	nodes      map[string]*nodeState

	nodeStore    NodeStore
	firstContact func(context.Context) error
}

type nodeState struct {
	streams  int
	applied  map[string]string
	nacks    int64
	lastNACK string
	// registered marks that durable registration and the pre-serve
	// refresh have succeeded for this node. Every request retries them
	// until then.
	registered bool
}

// NewServer builds an xDS server with no snapshot published yet. Streams
// opened before the first Publish block until a snapshot exists.
func NewServer(ctx context.Context) *Server {
	s := &Server{
		streams:    make(map[int64]string),
		streamCtxs: make(map[int64]context.Context),
		nodes:      make(map[string]*nodeState),
	}
	s.cache = cachev3.NewSnapshotCache(true, cachev3.IDHash{}, xdslog.LoggerFuncs{
		DebugFunc: func(format string, args ...interface{}) { slog.Debug(fmt.Sprintf(format, args...)) },
		InfoFunc:  func(format string, args ...interface{}) { slog.Info(fmt.Sprintf(format, args...)) },
		WarnFunc:  func(format string, args ...interface{}) { slog.Warn(fmt.Sprintf(format, args...)) },
		ErrorFunc: func(format string, args ...interface{}) { slog.Error(fmt.Sprintf(format, args...)) },
	})
	s.xds = serverv3.NewServer(ctx, s.cache, serverv3.CallbackFuncs{
		StreamOpenFunc:    s.onStreamOpen,
		StreamRequestFunc: s.onStreamRequest,
		StreamClosedFunc:  s.onStreamClosed,
		FetchRequestFunc:  s.onFetchRequest,
	})
	return s
}

// SetNodeStore durably records subscribers at first contact so the drain
// barrier can see them across replicas before they hold any config.
func (s *Server) SetNodeStore(store NodeStore) {
	if s == nil {
		return
	}
	s.nodeStore = store
}

// SetFirstContactHook installs the refresh that runs at first contact, after
// durable registration and before the request is answered.
func (s *Server) SetFirstContactHook(hook func(context.Context) error) {
	if s == nil {
		return
	}
	s.firstContact = hook
}

func (s *Server) GRPCServer() *grpc.Server {
	grpcServer := grpc.NewServer()
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(grpcServer, s.xds)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, s.xds)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, s.xds)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, s.xds)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, s.xds)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, s.xds)
	return grpcServer
}

// Publish atomically replaces the served snapshot for every connected node.
// Publishing the same version twice is a no-op.
func (s *Server) Publish(ctx context.Context, snapshot *Snapshot) {
	if s == nil || snapshot == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.Version == snapshot.Version {
		return
	}
	s.current = snapshot
	s.published = time.Now().UTC()
	for nodeID := range s.nodes {
		if err := s.cache.SetSnapshot(ctx, nodeID, snapshot.CacheSnapshot()); err != nil {
			slog.Warn("xds set snapshot failed", "node", nodeID, "error", err)
		}
	}
}

func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{Nodes: make(map[string]NodeStatus, len(s.nodes))}
	if s.current != nil {
		status.Version = s.current.Version
		status.Counts = s.current.Counts
		status.PublishedAt = s.published
		status.HasSnapshot = true
		status.RequiredTypes = s.current.RequiredTypes()
	}
	for nodeID, state := range s.nodes {
		applied := make(map[string]string, len(state.applied))
		for typ, version := range state.applied {
			applied[typ] = version
		}
		status.Nodes[nodeID] = NodeStatus{
			Applied:  applied,
			NACKs:    state.nacks,
			LastNACK: state.lastNACK,
		}
	}
	return status
}

func (s *Server) onStreamOpen(ctx context.Context, streamID int64, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamCtxs[streamID] = ctx
	return nil
}

func (s *Server) onStreamRequest(streamID int64, req *discoveryv3.DiscoveryRequest) error {
	if req == nil {
		return nil
	}
	nodeID := strings.TrimSpace(req.GetNode().GetId())
	s.mu.Lock()
	ctx := s.streamCtxs[streamID]
	if nodeID == "" {
		// Envoy sends node only on the first request of a stream.
		nodeID = s.streams[streamID]
	}
	s.mu.Unlock()
	if ctx == nil {
		return fmt.Errorf("xds request on untracked stream %d", streamID)
	}
	if nodeID == "" {
		return nil
	}
	s.observe(ctx, streamID, nodeID, req.GetTypeUrl(), req.GetVersionInfo(), req.GetErrorDetail())
	return s.atFirstContact(ctx, nodeID)
}

func (s *Server) onFetchRequest(ctx context.Context, req *discoveryv3.DiscoveryRequest) error {
	if req == nil {
		return nil
	}
	nodeID := strings.TrimSpace(req.GetNode().GetId())
	if nodeID == "" {
		return nil
	}
	s.observe(ctx, 0, nodeID, req.GetTypeUrl(), req.GetVersionInfo(), req.GetErrorDetail())
	return s.atFirstContact(ctx, nodeID)
}

// atFirstContact durably registers a subscriber and refreshes from the
// durable publication before the request is answered. Both steps repeat on
// every request until they succeed once. Failure fails the request closed.
func (s *Server) atFirstContact(ctx context.Context, nodeID string) error {
	s.mu.Lock()
	state := s.nodes[nodeID]
	registered := state != nil && state.registered
	s.mu.Unlock()
	if registered {
		return nil
	}
	if s.nodeStore != nil {
		if err := s.nodeStore.UpsertNodeObservations(ctx, []NodeObservation{{NodeID: nodeID}}); err != nil {
			return fmt.Errorf("register xds node %s: %w", nodeID, err)
		}
	}
	if s.firstContact != nil {
		if err := s.firstContact(ctx); err != nil {
			return fmt.Errorf("refresh before serving xds node %s: %w", nodeID, err)
		}
	}
	s.mu.Lock()
	if state := s.nodes[nodeID]; state != nil {
		state.registered = true
	}
	s.mu.Unlock()
	return nil
}

func (s *Server) observe(ctx context.Context, streamID int64, nodeID, typeURL, version string, errDetail *rpcstatus.Status) {
	if nodeID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.nodes[nodeID]
	if !ok {
		state = &nodeState{applied: make(map[string]string)}
		s.nodes[nodeID] = state
		if s.current != nil {
			if err := s.cache.SetSnapshot(ctx, nodeID, s.current.CacheSnapshot()); err != nil {
				slog.Warn("xds seed snapshot failed", "node", nodeID, "error", err)
			}
		}
	}
	if streamID != 0 {
		if prev, tracked := s.streams[streamID]; !tracked || prev != nodeID {
			s.streams[streamID] = nodeID
			state.streams++
		}
	}
	if errDetail != nil {
		// NACK: record the rejection and keep serving the published snapshot.
		state.nacks++
		state.lastNACK = errDetail.GetMessage()
		if typeURL != "" {
			slog.Warn("xds subscriber NACKed snapshot",
				"node", nodeID, "type", typeURL, "version", version, "error", errDetail.GetMessage())
		}
		return
	}
	if typeURL != "" && version != "" && s.current != nil && version == s.current.Version {
		state.applied[typeURL] = version
	}
}

func (s *Server) onStreamClosed(streamID int64, node *corev3.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streamCtxs, streamID)
	nodeID := ""
	if node != nil {
		nodeID = node.GetId()
	}
	if tracked, ok := s.streams[streamID]; ok {
		nodeID = tracked
		delete(s.streams, streamID)
	}
	if nodeID == "" {
		return
	}
	state, ok := s.nodes[nodeID]
	if !ok {
		return
	}
	if state.streams > 0 {
		state.streams--
	}
	if state.streams > 0 {
		return
	}
	delete(s.nodes, nodeID)
	s.cache.ClearSnapshot(nodeID)
}
