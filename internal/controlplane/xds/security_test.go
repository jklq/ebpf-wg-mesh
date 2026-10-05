package xds

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/identity/identitytest"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func securityEndpoint(t *testing.T, pki *identitytest.PKI) (*Server, string) {
	t.Helper()
	server := NewServer(context.Background())
	server.Publish(context.Background(), mustBuild(t, tlsInput()))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := server.GRPCServer(pki.Authority.XDSConfig(), pki.Authority.Revocations())
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	t.Cleanup(func() { _ = listener.Close() })
	return server, listener.Addr().String()
}

type controlledMembership struct {
	*fakeNodes
	active atomic.Bool
}

func (m *controlledMembership) NodeActive(context.Context, string) (bool, error) {
	return m.active.Load(), nil
}

func TestRetirementStopsSecretPushesOnExistingStream(t *testing.T) {
	t.Parallel()
	pki := identitytest.New(t)
	server, address := securityEndpoint(t, pki)
	members := &controlledMembership{fakeNodes: newFakeNodes()}
	members.active.Store(true)
	server.SetNodeStore(members)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := Dial(ctx, address, "envoy-1", pki.Client(t, identity.CallerIngress, "envoy-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Subscribe(ctx, resourcev3.SecretType)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Request(response.TypeUrl, response.VersionInfo, response.Nonce, nil); err != nil {
		t.Fatal(err)
	}
	waitForApplied(t, server, "envoy-1", []string{resourcev3.SecretType}, response.VersionInfo)
	members.active.Store(false)
	input := tlsInput()
	input.Backends = append(input.Backends, Backend{Domain: "new.example.com", Upstream: "10.0.0.99:80"})
	server.Publish(ctx, mustBuild(t, input))
	if response, err := client.RecvContext(ctx); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("retired stream response = %v, error = %v; want PermissionDenied", response, err)
	}
}

func TestUnauthorizedClientsCannotRetrieveConfigurationOrKeys(t *testing.T) {
	t.Parallel()
	pki := identitytest.New(t)
	server, address := securityEndpoint(t, pki)
	ingress := pki.Client(t, identity.CallerIngress, "envoy-1")
	noCert := ingress.Clone()
	noCert.Certificates = nil
	foreign := identitytest.New(t).Client(t, identity.CallerIngress, "envoy-1")
	foreign.RootCAs = ingress.RootCAs
	for _, tc := range []struct {
		name, node string
		creds      credentials.TransportCredentials
	}{
		{"plaintext", "envoy-1", insecure.NewCredentials()},
		{"no certificate", "envoy-1", credentials.NewTLS(noCert)},
		{"foreign CA", "envoy-1", credentials.NewTLS(foreign)},
		{"agent", "envoy-1", credentials.NewTLS(pki.Client(t, identity.CallerAgent, "envoy-1"))},
		{"builder", "envoy-1", credentials.NewTLS(pki.Client(t, identity.CallerBuilder, "envoy-1"))},
		{"dashboard", "envoy-1", credentials.NewTLS(pki.Client(t, identity.CallerDashboard, "envoy-1"))},
		{"spoofed node", "envoy-2", credentials.NewTLS(ingress)},
		{"missing node", "", credentials.NewTLS(ingress)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(tc.creds))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, api := range []struct{ service, resource string }{
				{"envoy.service.discovery.v3.AggregatedDiscoveryService/StreamAggregatedResources", resourcev3.SecretType},
				{"envoy.service.secret.v3.SecretDiscoveryService/StreamSecrets", resourcev3.SecretType},
				{"envoy.service.listener.v3.ListenerDiscoveryService/StreamListeners", resourcev3.ListenerType},
				{"envoy.service.cluster.v3.ClusterDiscoveryService/StreamClusters", resourcev3.ClusterType},
				{"envoy.service.route.v3.RouteDiscoveryService/StreamRoutes", resourcev3.RouteType},
				{"envoy.service.endpoint.v3.EndpointDiscoveryService/StreamEndpoints", resourcev3.EndpointType},
			} {
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/"+api.service)
				if err == nil {
					err = stream.SendMsg(&discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: tc.node}, TypeUrl: api.resource})
					if err == nil {
						err = stream.RecvMsg(&discoveryv3.DiscoveryResponse{})
					}
				}
				cancel()
				if err == nil {
					t.Fatalf("unauthorized client received %s", api.service)
				}
			}
			for _, method := range []string{
				"envoy.service.secret.v3.SecretDiscoveryService/FetchSecrets",
				"envoy.service.listener.v3.ListenerDiscoveryService/FetchListeners",
				"envoy.service.cluster.v3.ClusterDiscoveryService/FetchClusters",
				"envoy.service.route.v3.RouteDiscoveryService/FetchRoutes",
				"envoy.service.endpoint.v3.EndpointDiscoveryService/FetchEndpoints",
			} {
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				err := conn.Invoke(ctx, "/"+method, &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: tc.node}, TypeUrl: resourcev3.SecretType}, &discoveryv3.DiscoveryResponse{})
				cancel()
				if err == nil {
					t.Fatalf("unauthorized client received %s", method)
				}
			}
		})
	}
	if len(server.Status().Nodes) != 0 {
		t.Fatal("unauthorized clients entered the convergence barrier")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := Dial(ctx, address, "envoy-1", ingress)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Subscribe(ctx, resourcev3.SecretType)
	if err != nil || len(response.GetResources()) != 2 {
		t.Fatalf("authorized SDS = %v, %v", response, err)
	}
	if err := client.stream.Send(&discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "envoy-2"}, TypeUrl: resourcev3.SecretType}); err != nil {
		t.Fatal(err)
	}
	if response, err := client.RecvContext(ctx); err == nil {
		t.Fatalf("identity change received secrets: %v", response)
	}
}

func TestIngressCARotationAndRevocationOnExistingStream(t *testing.T) {
	t.Parallel()
	pki := identitytest.New(t)
	server, address := securityEndpoint(t, pki)
	old := pki.Client(t, identity.CallerIngress, "envoy-1")
	pki.Keys.Rotate(t, signkeys.ScopeInternalCA)
	current := pki.Client(t, identity.CallerIngress, "envoy-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, cfg := range []*tls.Config{old, current} {
		client, err := Dial(ctx, address, "envoy-1", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Subscribe(ctx, resourcev3.SecretType); err != nil {
			t.Fatal(err)
		}
		client.Close()
	}
	client, err := Dial(ctx, address, "envoy-1", current)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Subscribe(ctx, resourcev3.SecretType)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Request(response.TypeUrl, response.VersionInfo, response.Nonce, nil); err != nil {
		t.Fatal(err)
	}
	waitForApplied(t, server, "envoy-1", []string{resourcev3.SecretType}, response.VersionInfo)
	leaf, err := x509.ParseCertificate(current.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.Authority.Revocations().Add(leaf.SerialNumber.Text(16)); err != nil {
		t.Fatal(err)
	}
	input := tlsInput()
	input.Backends = append(input.Backends, Backend{Domain: "new.example.com", Upstream: "10.0.0.99:80"})
	server.Publish(ctx, mustBuild(t, input))
	if response, err := client.RecvContext(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked stream response = %v, error = %v; want Unauthenticated", response, err)
	}
}

func TestFetchRechecksRevocationAfterFirstContactRefresh(t *testing.T) {
	t.Parallel()
	pki := identitytest.New(t)
	server, address := securityEndpoint(t, pki)
	clientTLS := pki.Client(t, identity.CallerIngress, "envoy-1")
	leaf, err := x509.ParseCertificate(clientTLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	server.SetFirstContactHook(func(context.Context) error { return pki.Authority.Revocations().Add(leaf.SerialNumber.Text(16)) })
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/envoy.service.secret.v3.SecretDiscoveryService/FetchSecrets", &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "envoy-1"}, TypeUrl: resourcev3.SecretType}, &discoveryv3.DiscoveryResponse{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("fetch after revocation during refresh = %v; want Unauthenticated", err)
	}
}
