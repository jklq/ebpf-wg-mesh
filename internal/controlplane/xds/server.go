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

// NodeStatus is the ACK/NACK accounting for one connected Envoy.
type NodeStatus struct {
	// Applied maps xDS type URLs to the version the node ACKed.
	Applied map[string]string
	// NACKs counts rejected responses; LastNACK carries the latest message.
	NACKs    int64
	LastNACK string
}

// FullyApplied reports whether this node ACKed version across every
// required type. A node mid-apply, holding an older version, or holding a
// rejected response for any type is not fully applied: it may still route
// to endpoints the current version withdrew.
func (n NodeStatus) FullyApplied(version string) bool {
	for _, typeURL := range RequiredTypes {
		if n.Applied[typeURL] != version {
			return false
		}
	}
	return true
}

// Status is a point-in-time view of the served snapshot and its adoption.
type Status struct {
	Version     string
	Hash        string
	Counts      Counts
	PublishedAt time.Time
	HasSnapshot bool
	Nodes       map[string]NodeStatus
}

// Server is the xDS management server. It serves the latest published
// snapshot over ADS (plus per-type SotW) from an in-memory snapshot cache,
// tracks per-node ACK/NACK, and retains the last-known-good snapshot across
// NACKs, restarts, and reconnects: unknown nodes are seeded with the current
// snapshot on first contact, and a NACK only records the rejection.
type Server struct {
	cache cachev3.SnapshotCache
	xds   serverv3.Server

	mu        sync.Mutex
	current   *Snapshot
	published time.Time
	// streams maps stream IDs to node IDs so a node stays registered while
	// any of its streams (ADS multiplexes types on one; SotW uses several)
	// is open. It also attributes Envoy's node-less follow-up requests.
	streams map[int64]string
	nodes   map[string]*nodeState

	nodeStore    NodeStore
	firstContact func(context.Context) error
}

type nodeState struct {
	streams  int
	applied  map[string]string
	nacks    int64
	lastNACK string
}

// NewServer builds an xDS server with no snapshot published yet. Streams
// opened before the first Publish block until a snapshot exists, which is
// what lets a fresh Envoy converge after control-plane restart.
func NewServer(ctx context.Context) *Server {
	s := &Server{
		streams: make(map[int64]string),
		nodes:   make(map[string]*nodeState),
	}
	s.cache = cachev3.NewSnapshotCache(true, cachev3.IDHash{}, xdslog.LoggerFuncs{
		DebugFunc: func(format string, args ...interface{}) { slog.Debug(fmt.Sprintf(format, args...)) },
		InfoFunc:  func(format string, args ...interface{}) { slog.Info(fmt.Sprintf(format, args...)) },
		WarnFunc:  func(format string, args ...interface{}) { slog.Warn(fmt.Sprintf(format, args...)) },
		ErrorFunc: func(format string, args ...interface{}) { slog.Error(fmt.Sprintf(format, args...)) },
	})
	s.xds = serverv3.NewServer(ctx, s.cache, serverv3.CallbackFuncs{
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

// SetFirstContactHook installs the refresh that runs at first contact,
// after durable registration and before the request is answered: a fresh
// subscriber must never receive content older than the durable
// publication, or it could hold routes to withdrawn endpoints that the
// drain barrier then misses.
func (s *Server) SetFirstContactHook(hook func(context.Context) error) {
	if s == nil {
		return
	}
	s.firstContact = hook
}

// GRPCServer returns a gRPC server with every xDS service registered.
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

// Status copies the current publication and adoption state.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{Nodes: make(map[string]NodeStatus, len(s.nodes))}
	if s.current != nil {
		status.Version = s.current.Version
		status.Hash = s.current.Hash
		status.Counts = s.current.Counts
		status.PublishedAt = s.published
		status.HasSnapshot = true
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

func (s *Server) onStreamRequest(streamID int64, req *discoveryv3.DiscoveryRequest) error {
	if req == nil {
		return nil
	}
	nodeID := strings.TrimSpace(req.GetNode().GetId())
	if nodeID == "" {
		// Envoy sends node only on the first request of a stream: attribute
		// node-less ACKs and NACKs to the node that opened this stream
		// instead of dropping the standard apply path.
		s.mu.Lock()
		nodeID = s.streams[streamID]
		s.mu.Unlock()
	}
	if nodeID == "" {
		return nil
	}
	fresh := s.observe(streamID, nodeID, req.GetTypeUrl(), req.GetVersionInfo(), req.GetErrorDetail())
	return s.atFirstContact(nodeID, fresh, context.Background())
}

func (s *Server) onFetchRequest(ctx context.Context, req *discoveryv3.DiscoveryRequest) error {
	if req == nil {
		return nil
	}
	nodeID := strings.TrimSpace(req.GetNode().GetId())
	if nodeID == "" {
		return nil
	}
	fresh := s.observe(0, nodeID, req.GetTypeUrl(), req.GetVersionInfo(), req.GetErrorDetail())
	return s.atFirstContact(nodeID, fresh, ctx)
}

// atFirstContact durably registers a subscriber and refreshes from the
// durable publication before the request is answered. Both steps run
// before any config can reach the subscriber — registration first, so the
// drain barrier cannot miss it; refresh second, so it cannot receive
// content older than the row and hold routes the barrier believes gone.
// Failure fails the request closed rather than serving an untracked or
// stale subscriber.
func (s *Server) atFirstContact(nodeID string, fresh bool, ctx context.Context) error {
	if !fresh {
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
	return nil
}

func (s *Server) observe(streamID int64, nodeID, typeURL, version string, errDetail *rpcstatus.Status) bool {
	if nodeID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, known := s.nodes[nodeID]
	if !known {
		state = &nodeState{applied: make(map[string]string)}
		s.nodes[nodeID] = state
		if s.current != nil {
			// A (re)connecting Envoy converges immediately on the current
			// snapshot instead of waiting for the next publication.
			if err := s.cache.SetSnapshot(context.Background(), nodeID, s.current.CacheSnapshot()); err != nil {
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
		// NACK: record the rejection and keep serving the published
		// snapshot. Envoy retains its own last-known-good; the server must
		// not withdraw or roll back on a client it cannot parse for.
		state.nacks++
		state.lastNACK = errDetail.GetMessage()
		if typeURL != "" {
			slog.Warn("xds subscriber NACKed snapshot",
				"node", nodeID, "type", typeURL, "version", version, "error", errDetail.GetMessage())
		}
		return !known
	}
	if typeURL != "" && version != "" && s.current != nil && version == s.current.Version {
		state.applied[typeURL] = version
	}
	return !known
}

func (s *Server) onStreamClosed(streamID int64, node *corev3.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// Fully disconnected: drop adoption tracking and the cached snapshot so
	// node churn cannot grow memory without bound. A reconnecting node is
	// re-seeded from the current snapshot on its first request.
	delete(s.nodes, nodeID)
	s.cache.ClearSnapshot(nodeID)
}
