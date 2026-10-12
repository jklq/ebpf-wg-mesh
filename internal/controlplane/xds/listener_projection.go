package xds

import (
	"encoding/json"
	"fmt"
	"strconv"

	"ebof-wg-mesh/internal/continuity"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
)

const listenerPortsMetadata = continuity.ListenerPortsMetadata

// Each ingress process generation receives a private loopback projection of
// the same logical routing publication. Authentication and durable membership
// still use the unchanged ingress identity. Canonical route ports, TLS secrets
// and publication versions are never rewritten.
func projection(node *corev3.Node) (string, map[uint32]uint32, error) {
	id := node.GetId()
	value := node.GetMetadata().GetFields()[listenerPortsMetadata]
	if value == nil {
		return id, nil, nil
	}
	fields := value.GetStructValue().GetFields()
	if len(fields) == 0 || len(fields) > 8 {
		return "", nil, fmt.Errorf("invalid ingress listener projection")
	}
	ports := map[uint32]uint32{}
	seen := map[uint32]bool{}
	for key, value := range fields {
		from, err := strconv.ParseUint(key, 10, 16)
		to := value.GetNumberValue()
		if err != nil || from == 0 || to < 1024 || to > 65535 || float64(uint32(to)) != to || seen[uint32(to)] {
			return "", nil, fmt.Errorf("invalid ingress listener projection port")
		}
		ports[uint32(from)] = uint32(to)
		seen[uint32(to)] = true
	}
	b, _ := json.Marshal(ports)
	return id + "@" + continuity.Digest(b), ports, nil
}

type projectionHash struct{}

func (projectionHash) ID(node *corev3.Node) string {
	key, _, err := projection(node)
	if err != nil {
		return "invalid"
	}
	return key
}

func (s *Snapshot) forPorts(ports map[uint32]uint32) (*cachev3.Snapshot, error) {
	if len(ports) == 0 {
		return s.CacheSnapshot(), nil
	}
	resources := map[resourcev3.Type][]cachetypes.Resource{}
	seen := map[uint32]bool{}
	for _, typ := range []resourcev3.Type{resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.EndpointType, resourcev3.RouteType, resourcev3.SecretType} {
		for _, item := range s.cache.GetResources(typ) {
			if typ == resourcev3.ListenerType {
				listener := proto.Clone(item).(*listenerv3.Listener)
				socket := listener.GetAddress().GetSocketAddress()
				port, ok := ports[socket.GetPortValue()]
				if !ok {
					return nil, fmt.Errorf("ingress projection lacks listener port")
				}
				// IPv4/IPv6 public listeners share one dual-stack stable frontend.
				// Their identical logical routing needs only one private listener.
				if seen[port] {
					continue
				}
				seen[port] = true
				socket.Address = "127.0.0.1"
				socket.PortSpecifier = &corev3.SocketAddress_PortValue{PortValue: port}
				listener.AdditionalAddresses = nil
				item = listener
			}
			resources[typ] = append(resources[typ], item)
		}
	}
	return cachev3.NewSnapshot(s.Version, resources)
}

func (s *Server) ensureProjection(ctxNode *corev3.Node) error {
	key, ports, err := projection(ctxNode)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[key]; ok {
		return nil
	}
	s.nodes[key] = &nodeState{identity: ctxNode.GetId(), ports: ports, applied: map[string]string{}}
	return nil
}
