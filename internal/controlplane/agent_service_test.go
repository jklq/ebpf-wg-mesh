package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func peerCtx(ip string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 54321},
	})
}

type stringPeerAddr string

func (a stringPeerAddr) Network() string { return "tcp" }
func (a stringPeerAddr) String() string  { return string(a) }

func TestValidateAgentEndpointAgainstPeer(t *testing.T) {
	hello := func(endpoint string) *agentv1.AgentHello {
		return &agentv1.AgentHello{WireguardEndpoint: endpoint, AdvertiseAddr: "2001:db8::10"}
	}

	t.Run("same-family match allowed", func(t *testing.T) {
		if err := validateAgentEndpointAgainstPeer(peerCtx("203.0.113.10"), hello("203.0.113.10:51820")); err != nil {
			t.Fatalf("direct path: %v", err)
		}
	})

	t.Run("ServeHTTP string peer address allowed", func(t *testing.T) {
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: stringPeerAddr("203.0.113.10:54321")})
		if err := validateAgentEndpointAgainstPeer(ctx, hello("203.0.113.10:51820")); err != nil {
			t.Fatalf("string peer: %v", err)
		}
	})

	t.Run("self-reported advertise address does not bypass peer check", func(t *testing.T) {
		h := hello("[2001:db8::10]:51820")
		if err := validateAgentEndpointAgainstPeer(peerCtx("2001:db8::20"), h); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("unverified endpoint: %v", err)
		}
	})

	t.Run("unrelated endpoint rejected", func(t *testing.T) {
		err := validateAgentEndpointAgainstPeer(peerCtx("203.0.113.10"), hello("198.51.100.1:51820"))
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("unrelated endpoint: %v", err)
		}
	})

	t.Run("loopback skips anti-spoof", func(t *testing.T) {
		if err := validateAgentEndpointAgainstPeer(peerCtx("127.0.0.1"), hello("198.51.100.1:51820")); err != nil {
			t.Fatalf("loopback: %v", err)
		}
	})

	t.Run("family-mismatch dual-stack allowed when both usable", func(t *testing.T) {
		if err := validateAgentEndpointAgainstPeer(peerCtx("192.0.2.10"), hello("[2001:db8::10]:51820")); err != nil {
			t.Fatalf("dual-stack: %v", err)
		}
	})

	t.Run("family-mismatch rejected when endpoint is not usable unicast", func(t *testing.T) {
		err := validateAgentEndpointAgainstPeer(peerCtx("2001:db8::10"), hello("[fe80::1]:51820"))
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("link-local endpoint: %v", err)
		}
	})

	t.Run("invalid port rejected", func(t *testing.T) {
		for _, endpoint := range []string{"192.0.2.10:0", "192.0.2.10:70000"} {
			if err := validateAgentEndpointAgainstPeer(peerCtx("203.0.113.10"), hello(endpoint)); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("endpoint %q: %v", endpoint, err)
			}
		}
	})
}

type stubLiveOwner struct {
	held bool
	addr string
	err  error
}

func (s stubLiveOwner) Lookup(context.Context) (bool, string, error) {
	return s.held, s.addr, s.err
}

func TestAgentServiceRedirectsNonOwnerRPCs(t *testing.T) {
	t.Parallel()
	service := NewAgentService(nil, nil, nil, nil, nil, nil, true, "agent-trusted", "dashboard-1",
		WithLiveOwner(stubLiveOwner{addr: "owner:9443"}))
	_, err := service.Enroll(context.Background(), &agentv1.EnrollRequest{AgentId: "agent-1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Enroll status = %v", err)
	}
	if got, ok := parseAgentLiveOwner(err); !ok || got != "owner:9443" {
		t.Fatalf("Enroll redirect = %q, %v", got, err)
	}
	_, err = service.IssueManagedDashboardCertificate(context.Background(), &agentv1.ManagedDashboardCertificateRequest{AgentId: "agent-trusted"})
	if got, ok := parseAgentLiveOwner(err); !ok || got != "owner:9443" {
		t.Fatalf("dashboard redirect = %q, %v", got, err)
	}
	unavailable := NewAgentService(nil, nil, nil, nil, nil, nil, false, "", "", WithLiveOwner(stubLiveOwner{}))
	_, err = unavailable.Enroll(context.Background(), &agentv1.EnrollRequest{AgentId: "agent-1"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("missing owner status = %v", err)
	}
}

func parseAgentLiveOwner(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || !strings.HasPrefix(st.Message(), agentv1.LiveOwnerRedirectPrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(st.Message(), agentv1.LiveOwnerRedirectPrefix)), true
}

func TestLeaseOwnerWatchCoalescesTransitionsAndCloses(t *testing.T) {
	t.Parallel()
	m := NewLeaseManager(nil, time.Second, time.Millisecond)
	changed, stop := m.Watch(SingletonLeaseName)
	m.notifyChanged(SingletonLeaseName)
	m.notifyChanged(SingletonLeaseName)

	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("lease transition was not delivered")
	}
	select {
	case <-changed:
		t.Fatal("lease transitions were not coalesced")
	default:
	}
	stop()
	if _, ok := <-changed; ok {
		t.Fatal("lease watch remained open after stop")
	}
}

func TestAgentServiceAttachesExactPullCredentialOnlyToPlatformImages(t *testing.T) {
	t.Parallel()
	cfg := config.RegistryConfig{
		Host:                 "registry.example.test:5000",
		NamespacePrefix:      "mesh",
		TokenIssuer:          "registry-test",
		TokenService:         "registry.example.test:5000",
		CredentialTTLSeconds: 300,
	}
	auth, err := registry.NewAuth(context.Background(), cfg, signkeystest.New(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := &AgentService{registry: registry.NewPolicy(cfg, auth)}
	state := &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{
		{AllocationId: "allocation-1", ServiceId: "service-1", EnvironmentId: "environment-1", Spec: &platformv1.ResolvedServiceSpec{Image: "registry.example.test:5000/mesh/project-1/environment-1/build-1/service-1@sha256:" + strings.Repeat("a", 64)}},
		{AllocationId: "allocation-2", ServiceId: "service-2", EnvironmentId: "environment-2", Spec: &platformv1.ResolvedServiceSpec{Image: "docker.io/library/nginx:latest"}},
	}}
	creds, err := service.pullCredentialsForAgent(context.Background(), "agent-1", state)
	if err != nil {
		t.Fatal(err)
	}
	if creds.GetCredentialsVersion() == "" {
		t.Fatal("credentials version is empty")
	}
	if len(creds.GetCredentials()) != 1 || creds.GetCredentials()[0].GetAllocationId() != "allocation-1" {
		t.Fatalf("unexpected credentials %+v", creds.GetCredentials())
	}
	managed := creds.GetCredentials()[0]
	if managed.GetUsername() == "" || managed.GetPassword() == "" {
		t.Fatal("platform image did not receive pull credentials")
	}
	granted := tokenAccessForCredential(t, auth, cfg.TokenService, managed.GetUsername(), managed.GetPassword(), "repository:mesh/project-1/environment-1/build-1/service-1:pull")
	if len(granted) != 1 || granted[0].Name != "mesh/project-1/environment-1/build-1/service-1" || !slices.Equal(granted[0].Actions, []string{"pull"}) {
		t.Fatalf("unexpected pull scope %+v", granted)
	}
	if denied := tokenAccessForCredential(t, auth, cfg.TokenService, managed.GetUsername(), managed.GetPassword(), "repository:mesh/project-1/environment-1/build-1/service-1:push"); len(denied) != 0 {
		t.Fatalf("pull credential granted push: %+v", denied)
	}
	// Allocation messages must not carry credentials on the wire.
	if state.Services[0].GetRegistryUsername() != "" || state.Services[0].GetRegistryPassword() != "" {
		t.Fatal("checkpoint still carries pull credentials")
	}
	foreign := &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{{AllocationId: "allocation-3", EnvironmentId: "environment-2", ServiceId: "service-2", Spec: &platformv1.ResolvedServiceSpec{Image: "registry.example.test:5000/mesh/project-1/environment-1/build-1/service-1@sha256:" + strings.Repeat("b", 64)}}}}
	if _, err := service.pullCredentialsForAgent(context.Background(), "agent-1", foreign); err == nil {
		t.Fatal("expected a sibling platform repository to be rejected")
	}
}

type registryTokenAccess struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

func tokenAccessForCredential(t *testing.T, auth *registry.Auth, tokenService, username, password, scope string) []registryTokenAccess {
	t.Helper()
	query := url.Values{"service": {tokenService}, "scope": {scope}}
	req := httptest.NewRequest(http.MethodGet, registry.TokenPath+"?"+query.Encode(), nil)
	req.SetBasicAuth(username, password)
	resp := httptest.NewRecorder()
	auth.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("token response = %d: %s", resp.Code, resp.Body.String())
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(body.Token, claims); err != nil {
		t.Fatalf("parse registry token: %v", err)
	}
	raw, err := json.Marshal(claims["access"])
	if err != nil {
		t.Fatal(err)
	}
	var access []registryTokenAccess
	if err := json.Unmarshal(raw, &access); err != nil {
		t.Fatal(err)
	}
	return access
}
