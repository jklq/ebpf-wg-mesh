//go:build integration

package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestCARotationAcrossLiveSessionRenewalAndRestart walks a full internal-CA
// rotation against live servers: an established agent session survives,
// pre- and post-renewal agents connect through the overlap, a replica that
// starts mid-rotation accepts both generations, and the retiring identity
// stops working after finish while a restarted replica serves the new CA.
func TestCARotationAcrossLiveSessionRenewalAndRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Three agents: one holds a pre-rotation session open through the
	// whole rotation, one renews mid-rotation, one stays stale.
	const liveAgentID, renewedAgentID, staleAgentID = "agent-rotation-live", "agent-rotation-renewed", "agent-rotation-stale"
	cp1 := startSystemControlPlane(t, systemControlPlaneOptions{
		withDashboard: true,
		bootstrapTokens: []config.AgentBootstrapToken{
			{AgentID: liveAgentID, Token: "rotation-bootstrap-live"},
			{AgentID: renewedAgentID, Token: "rotation-bootstrap-renewed"},
			{AgentID: staleAgentID, Token: "rotation-bootstrap-stale"},
			{AgentID: "system-test-agent", Token: "system-test-bootstrap"},
		},
	})
	keys := cp1.server.SigningKeys()

	clusterBefore, err := cp1.server.authority.ClusterIdentity(ctx)
	if err != nil {
		t.Fatalf("ClusterIdentity: %v", err)
	}
	certLive := enrollAgentTLS(t, cp1.server, liveAgentID, "rotation-bootstrap-live")
	certRenewedBefore := enrollAgentTLS(t, cp1.server, renewedAgentID, "rotation-bootstrap-renewed")
	certStale := enrollAgentTLS(t, cp1.server, staleAgentID, "rotation-bootstrap-stale")
	liveStream, liveCancel := openAgentSync(t, cp1.server, certLive, rotationHello(liveAgentID, "rotation-live-session", ""))
	defer liveCancel()
	recvDesiredState(t, liveStream)

	// Rotate. The cluster identity flips, but both generations verify.
	activeCA, retiringCA, err := keys.RotateStart(ctx, signkeys.ScopeInternalCA, signkeys.RotateOptions{})
	if err != nil {
		t.Fatalf("RotateStart: %v", err)
	}
	clusterAfter, err := cp1.server.authority.ClusterIdentity(ctx)
	if err != nil {
		t.Fatalf("ClusterIdentity: %v", err)
	}
	if clusterAfter == clusterBefore {
		t.Fatal("cluster identity did not flip at rotate-start")
	}

	// A replica that starts mid-rotation boots and accepts both identities.
	cp2 := startSystemControlPlane(t, systemControlPlaneOptions{
		databaseURL: cp1.cfg.Database.URL,
		stateDir:    cp1.cfg.StateDir,
		standby:     true,
	})
	for name, id := range map[string]string{"retiring": clusterBefore, "active": clusterAfter} {
		ok, err := cp2.server.authority.VerifyClusterID(ctx, id)
		if err != nil || !ok {
			t.Fatalf("mid-rotation replica rejects %s identity: ok=%v err=%v", name, ok, err)
		}
	}
	if _, err := cp2.server.EnsureDashboardClientIdentity(ctx, systemTestDashboardID); err != nil {
		t.Fatalf("mid-rotation replica dashboard identity: %v", err)
	}

	// Renewal over the old client certificate issues under the new CA.
	certAfter := renewAgentCertificate(t, cp1.server, renewedAgentID, certRenewedBefore)
	activeMat, err := keys.Active(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	renewedLeaf := mustParseCertificate(t, pemEncodeLeaf(t, certAfter))
	if err := renewedLeaf.CheckSignatureFrom(activeMat.Cert); err != nil {
		t.Fatalf("renewed certificate is not signed by the active CA: %v", err)
	}

	// Both generations open new sessions through the overlap.
	renewedStream, renewedCancel := openAgentSync(t, cp1.server, certAfter, rotationHello(renewedAgentID, "rotation-renewed-session", clusterAfter))
	defer renewedCancel()
	recvDesiredState(t, renewedStream)
	staleStream, staleCancel := openAgentSync(t, cp1.server, certStale, rotationHello(staleAgentID, "rotation-stale-session", clusterBefore))
	defer staleCancel()
	recvDesiredState(t, staleStream)

	// The pre-rotation live session keeps receiving: the server echoes the
	// hello's verified identity, so the rotation mid-stream is invisible.
	userCtx := userContext(t, cp1, ctx, "rotation-user")
	project, err := cp1.dashboard.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "rotation-live"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	environments, err := cp1.dashboard.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil || len(environments.GetEnvironments()) != 1 {
		t.Fatalf("ListEnvironments: %+v: %v", environments, err)
	}
	environmentID := environments.GetEnvironments()[0].GetId()
	if _, err := cp1.dashboard.CreateService(userCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{Name: "web", Spec: directImageServiceSpec(pinnedImage("d"), &platformv1.ServiceRuntime{
			CpuMillis: 250, MemoryMebibytes: 256, Ports: runtimePortsFromInts([]int32{8080}),
		})},
	}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if _, err := cp1.dashboard.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID}); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	updated := recvDesiredState(t, liveStream)
	if len(updated.GetServices()) != 1 {
		t.Fatalf("live session services: got %d, want 1", len(updated.GetServices()))
	}
	if updated.GetClusterId() != clusterBefore {
		t.Fatalf("live session cluster echo = %q, want pre-rotation identity", updated.GetClusterId())
	}

	// Finish once the overlap elapsed. The retiring generation stops.
	if retiringCA.RetiredAt == nil {
		t.Fatal("retiring record has no retired_at")
	}
	floor, err := signkeys.MinOverlapForScope(signkeys.ScopeInternalCA)
	if err != nil {
		t.Fatalf("MinOverlapForScope: %v", err)
	}
	if _, err := keys.RotateFinish(ctx, signkeys.ScopeInternalCA, signkeys.FinishOptions{}); !errors.Is(err, signkeys.ErrOverlapNotElapsed) {
		t.Fatalf("early RotateFinish = %v, want ErrOverlapNotElapsed", err)
	}
	if _, err := keys.RotateFinish(ctx, signkeys.ScopeInternalCA, signkeys.FinishOptions{Now: retiringCA.RetiredAt.Add(floor).Add(time.Minute)}); err != nil {
		t.Fatalf("RotateFinish: %v", err)
	}
	if ok, err := cp1.server.authority.VerifyClusterID(ctx, clusterBefore); err != nil || ok {
		t.Fatalf("retiring identity still trusted: ok=%v err=%v", ok, err)
	}
	// Every replica flips its server leaf to the new CA on its own; the
	// test triggers the same refresh the per-minute loop runs. Until a
	// replica flips, post-finish clients holding active-only roots cannot
	// dial it (agents retry through the sub-minute window).
	if err := cp1.server.authority.RefreshServerCertificate(ctx); err != nil {
		t.Fatalf("RefreshServerCertificate: %v", err)
	}
	fresh, err := cp2.server.authority.ClusterIdentity(ctx)
	if err != nil || fresh != clusterAfter {
		t.Fatalf("mid-rotation replica identity = %q, %v; want %q", fresh, err, clusterAfter)
	}

	// Post-finish, the stale generation cannot connect: the trust bundle
	// no longer contains its CA, so the server rejects the client
	// certificate on every handshake and the dial never establishes.
	staleIdentity, err := cp1.server.EnsureDashboardClientIdentity(ctx, systemTestDashboardID)
	if err != nil {
		t.Fatalf("dashboard identity for CA: %v", err)
	}
	staleRoots := x509.NewCertPool()
	if !staleRoots.AppendCertsFromPEM(staleIdentity.CAPEM) {
		t.Fatal("ca")
	}
	staleDialCtx, staleDialCancel := context.WithTimeout(ctx, 15*time.Second)
	defer staleDialCancel()
	staleConn, err := grpc.DialContext(staleDialCtx, cp1.server.InternalAddr(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{certStale},
			RootCAs:      staleRoots,
			ServerName:   "localhost",
			MinVersion:   tls.VersionTLS13,
		})),
		grpc.WithBlock())
	if err == nil {
		_ = staleConn.Close()
		t.Fatal("post-finish stale client certificate connected")
	}
	// A valid certificate paired with a stale cluster identity is
	// rejected at hello.
	wrongIDStream, wrongIDCancel := openAgentSync(t, cp1.server, certAfter, rotationHello(renewedAgentID, "rotation-wrong-id-session", clusterBefore))
	defer wrongIDCancel()
	if _, err := wrongIDStream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("post-finish stale cluster hello = %v, want FailedPrecondition", err)
	}
	_ = activeCA

	// A replica that restarts after finish serves the new generation. The
	// mid-rotation witness stops first so the restarted replica wins the
	// singleton lease; starting it non-standby waits for that lease.
	cp1.stop()
	cp2.stop()
	cp3 := startSystemControlPlane(t, systemControlPlaneOptions{
		databaseURL: cp1.cfg.Database.URL,
		stateDir:    cp1.cfg.StateDir,
	})
	postRestart, postRestartCancel := openAgentSync(t, cp3.server, certAfter, rotationHello(renewedAgentID, "rotation-restart-session", clusterAfter))
	defer postRestartCancel()
	recvDesiredState(t, postRestart)
}

// TestRegistryRotationAcrossTokenExchange walks a registry rotation through
// the token endpoint: capabilities signed by either key exchange through
// the overlap, minted tokens chain to the refreshed bundle, and the
// retiring key stops verifying after finish.
func TestRegistryRotationAcrossTokenExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cp1 := startSystemControlPlane(t, systemControlPlaneOptions{withDashboard: true})
	// The harness boots without a registry, so its scope is uninitialized;
	// initialize it the way the operator does when enabling the registry.
	if _, err := cp1.server.SigningKeys().EnsureActiveKey(ctx, signkeys.ScopeRegistry, signkeys.EnsureOptions{}); err != nil {
		t.Fatalf("EnsureActiveKey(registry): %v", err)
	}
	cfg := config.RegistryConfig{
		Host: "registry.rotation.test:5000", NamespacePrefix: "mesh",
		AuthListen: "127.0.0.1:0", TokenIssuer: "rotation-test",
		TokenService: "registry.rotation.test:5000", CredentialTTLSeconds: 300,
		PullCredentialTTLSeconds: 48 * 3600,
	}
	auth, err := registry.NewAuth(ctx, cfg, cp1.server.SigningKeys(), t.TempDir())
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	// A second service handle over the same database is a second replica's
	// view of the shared key state.
	replicaKeys := signkeys.New(cp1.server.store.db, cp1.server.store.secrets.Registry())
	replicaAuth, err := registry.NewAuth(ctx, cfg, replicaKeys, t.TempDir())
	if err != nil {
		t.Fatalf("NewAuth replica: %v", err)
	}

	expires := time.Now().UTC().Add(48 * time.Hour)
	repository := "mesh/project-1/environment-1/build-1/service-1"
	userBefore, passBefore, err := auth.MintCredential(ctx, "agent-1", repository, []string{"pull"}, &expires)
	if err != nil {
		t.Fatalf("MintCredential: %v", err)
	}
	if _, _, err := cp1.server.SigningKeys().RotateStart(ctx, signkeys.ScopeRegistry, signkeys.RotateOptions{}); err != nil {
		t.Fatalf("RotateStart: %v", err)
	}
	userAfter, passAfter, err := auth.MintCredential(ctx, "agent-1", repository, []string{"pull"}, &expires)
	if err != nil {
		t.Fatalf("MintCredential: %v", err)
	}
	// Either generation exchanges on either replica through the overlap.
	tokenBefore := exchangeRegistryToken(t, replicaAuth, userBefore, passBefore, cfg.TokenService, "repository:"+repository+":pull")
	tokenAfter := exchangeRegistryToken(t, replicaAuth, userAfter, passAfter, cfg.TokenService, "repository:"+repository+":pull")

	// Minted tokens chain to the refreshed bundle the way the registry
	// daemon verifies them against its rootcertbundle.
	if err := auth.RefreshTrustBundle(ctx); err != nil {
		t.Fatalf("RefreshTrustBundle: %v", err)
	}
	bundle, err := cp1.server.SigningKeys().PublicBundle(ctx, signkeys.ScopeRegistry)
	if err != nil {
		t.Fatalf("PublicBundle: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		t.Fatal("append registry bundle")
	}
	for name, raw := range map[string]string{"before": tokenBefore, "after": tokenAfter} {
		parser := jwt.NewParser()
		token, _, err := parser.ParseUnverified(raw, &jwt.RegisteredClaims{})
		if err != nil {
			t.Fatalf("parse token (%s): %v", name, err)
		}
		x5c, _ := token.Header["x5c"].([]any)
		if len(x5c) != 1 {
			t.Fatalf("token (%s) carries %d x5c certificates, want 1", name, len(x5c))
		}
		der, err := base64.StdEncoding.DecodeString(x5c[0].(string))
		if err != nil {
			t.Fatalf("decode x5c (%s): %v", name, err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse x5c (%s): %v", name, err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
			t.Fatalf("verify token chain (%s): %v", name, err)
		}
	}

	floor, err := signkeys.MinOverlapForScope(signkeys.ScopeRegistry)
	if err != nil {
		t.Fatalf("MinOverlapForScope: %v", err)
	}
	records, err := cp1.server.SigningKeys().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var retiredAt time.Time
	for _, rec := range records {
		if rec.Scope == signkeys.ScopeRegistry && rec.State == signkeys.KeyStateRetiring && rec.RetiredAt != nil {
			retiredAt = *rec.RetiredAt
		}
	}
	if retiredAt.IsZero() {
		t.Fatal("retiring registry key has no retired_at")
	}
	if _, err := cp1.server.SigningKeys().RotateFinish(ctx, signkeys.ScopeRegistry,
		signkeys.FinishOptions{Now: retiredAt.Add(floor).Add(time.Minute)}); err != nil {
		t.Fatalf("RotateFinish: %v", err)
	}
	exchangeRegistryTokenExpect(t, replicaAuth, userAfter, passAfter, cfg.TokenService, http.StatusOK)
	exchangeRegistryTokenExpect(t, replicaAuth, userBefore, passBefore, cfg.TokenService, http.StatusUnauthorized)
}

// TestDashboardSecretRotationAcrossLiveRPCs walks the dashboard-held scopes:
// user assertions verify against both secrets through the overlap over live
// RPCs, and the session scope exports both generations with its 31-day
// floor. The console verifies sessions itself; this test proves the
// exported data supports dual-secret verification per the handoff contract.
func TestDashboardSecretRotationAcrossLiveRPCs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cp1 := startSystemControlPlane(t, systemControlPlaneOptions{withDashboard: true})
	keys := cp1.server.SigningKeys()

	secretBefore, err := keys.ActiveSecret(ctx, signkeys.ScopeUserAssertion)
	if err != nil {
		t.Fatalf("ActiveSecret: %v", err)
	}
	assertionBefore := signedLiveUserAssertionWithSecret(t, secretBefore, "rotation-user")
	if _, err := cp1.dashboard.ListProjects(metadata.AppendToOutgoingContext(ctx, userAssertionHeader, assertionBefore), &platformv1.ListProjectsRequest{}); err != nil {
		t.Fatalf("pre-rotation RPC: %v", err)
	}
	_, retiring, err := keys.RotateStart(ctx, signkeys.ScopeUserAssertion, signkeys.RotateOptions{})
	if err != nil {
		t.Fatalf("RotateStart: %v", err)
	}
	secretAfter, err := keys.ActiveSecret(ctx, signkeys.ScopeUserAssertion)
	if err != nil {
		t.Fatalf("ActiveSecret: %v", err)
	}
	assertionAfter := signedLiveUserAssertionWithSecret(t, secretAfter, "rotation-user")
	for name, assertion := range map[string]string{"retiring": assertionBefore, "active": assertionAfter} {
		if _, err := cp1.dashboard.ListProjects(metadata.AppendToOutgoingContext(ctx, userAssertionHeader, assertion), &platformv1.ListProjectsRequest{}); err != nil {
			t.Fatalf("%s RPC through overlap: %v", name, err)
		}
	}
	floor, err := signkeys.MinOverlapForScope(signkeys.ScopeUserAssertion)
	if err != nil {
		t.Fatalf("MinOverlapForScope: %v", err)
	}
	if _, err := keys.RotateFinish(ctx, signkeys.ScopeUserAssertion,
		signkeys.FinishOptions{Now: retiring.RetiredAt.Add(floor).Add(time.Minute)}); err != nil {
		t.Fatalf("RotateFinish: %v", err)
	}
	if _, err := cp1.dashboard.ListProjects(metadata.AppendToOutgoingContext(ctx, userAssertionHeader, assertionAfter), &platformv1.ListProjectsRequest{}); err != nil {
		t.Fatalf("post-finish active RPC: %v", err)
	}
	if _, err := cp1.dashboard.ListProjects(metadata.AppendToOutgoingContext(ctx, userAssertionHeader, assertionBefore), &platformv1.ListProjectsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("post-finish retiring RPC = %v, want Unauthenticated", err)
	}

	// Sessions: the control plane never verifies, so the test mirrors the
	// console's HS256 check against both exported generations.
	sessionBefore, err := keys.ActiveSecret(ctx, signkeys.ScopeDashboardSession)
	if err != nil {
		t.Fatalf("ActiveSecret: %v", err)
	}
	_, sessionRetiring, err := keys.RotateStart(ctx, signkeys.ScopeDashboardSession, signkeys.RotateOptions{})
	if err != nil {
		t.Fatalf("RotateStart: %v", err)
	}
	sessionAfter, err := keys.ActiveSecret(ctx, signkeys.ScopeDashboardSession)
	if err != nil {
		t.Fatalf("ActiveSecret: %v", err)
	}
	payload := "rotation-session-payload"
	macBefore := hmac.New(sha256.New, sessionBefore)
	macBefore.Write([]byte(payload))
	signature := macBefore.Sum(nil)
	for name, secret := range map[string][]byte{"retiring": sessionBefore, "active": sessionAfter} {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(payload))
		if name == "retiring" && !hmac.Equal(mac.Sum(nil), signature) {
			t.Fatal("pre-rotation session no longer verifies against the retiring secret")
		}
		if name == "active" && hmac.Equal(mac.Sum(nil), signature) {
			t.Fatal("rotated session secret is identical to the retiring one")
		}
	}
	sessionFloor, err := signkeys.MinOverlapForScope(signkeys.ScopeDashboardSession)
	if err != nil {
		t.Fatalf("MinOverlapForScope: %v", err)
	}
	if sessionFloor != 31*24*time.Hour {
		t.Fatalf("session overlap floor = %s, want 744h", sessionFloor)
	}
	if _, err := keys.RotateFinish(ctx, signkeys.ScopeDashboardSession,
		signkeys.FinishOptions{Now: sessionRetiring.RetiredAt.Add(30 * 24 * time.Hour)}); !errors.Is(err, signkeys.ErrOverlapNotElapsed) {
		t.Fatalf("30-day session finish = %v, want ErrOverlapNotElapsed", err)
	}
	if _, err := keys.RotateFinish(ctx, signkeys.ScopeDashboardSession,
		signkeys.FinishOptions{Now: sessionRetiring.RetiredAt.Add(32 * 24 * time.Hour)}); err != nil {
		t.Fatalf("RotateFinish: %v", err)
	}
}

// TestEnvelopeDeleteRefusesSigningKeyWrapping proves envelope rotation stays
// safe: a retired envelope key that still wraps signing keys cannot be
// deleted until rewrap moves them.
func TestEnvelopeDeleteRefusesSigningKeyWrapping(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ensureTestSigningKeys(t, store)

	first, err := store.secrets.Registry().ActiveKey(ctx)
	if err != nil {
		t.Fatalf("ActiveKey: %v", err)
	}
	second, err := store.secrets.Registry().Activate(ctx, provisionVersion(t, store.secrets))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := store.secrets.Registry().DeleteKey(ctx, first.ID); !errors.Is(err, secretkeys.ErrKeyHasLiveCiphertext) {
		t.Fatalf("DeleteKey(before rewrap) = %v, want ErrKeyHasLiveCiphertext", err)
	}
	signing := signkeys.New(store.db, store.secrets.Registry())
	if _, err := signing.RewrapAll(ctx); err != nil {
		t.Fatalf("RewrapAll: %v", err)
	}
	if _, err := store.secrets.DEKs().RewrapAll(ctx); err != nil {
		t.Fatalf("DEK rewrap: %v", err)
	}
	if err := store.secrets.Registry().DeleteKey(ctx, first.ID); err != nil {
		t.Fatalf("DeleteKey(after rewrap): %v", err)
	}
	if err := store.secrets.Registry().DeleteKey(ctx, second.ID); !errors.Is(err, secretkeys.ErrKeyIsActive) {
		t.Fatalf("DeleteKey(active) = %v, want ErrKeyIsActive", err)
	}
}

// renewAgentCertificate exercises the real renewal path: a fresh CSR over a
// connection authenticated by the current client certificate.
func renewAgentCertificate(t *testing.T, server *Server, agentID string, current tls.Certificate) tls.Certificate {
	t.Helper()
	identity, err := server.EnsureDashboardClientIdentity(context.Background(), systemTestDashboardID)
	if err != nil {
		t.Fatalf("dashboard identity for CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(identity.CAPEM) {
		t.Fatal("ca")
	}
	conn := dialLiveTLS(t, server.InternalAddr(), &tls.Config{
		Certificates: []tls.Certificate{current},
		RootCAs:      roots,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	})
	defer conn.Close()
	key, csrPEM := newAgentCSR(t, agentID)
	enrolled, err := agentv1.NewAgentControlClient(conn).Enroll(context.Background(), &agentv1.EnrollRequest{
		AgentId: agentID,
		CsrPem:  string(csrPEM),
	})
	if err != nil {
		t.Fatalf("renew Enroll(%s): %v", agentID, err)
	}
	return tls.Certificate{
		Certificate: [][]byte{mustDecodePEMBlock(t, enrolled.GetCertPem(), "CERTIFICATE")},
		PrivateKey:  key,
	}
}

func pemEncodeLeaf(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	if len(cert.Certificate) == 0 {
		t.Fatal("certificate has no leaf")
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
}

func exchangeRegistryToken(t *testing.T, auth *registry.Auth, username, password, service, scope string) string {
	t.Helper()
	query := url.Values{"service": {service}, "scope": {scope}}
	req := httptest.NewRequest(http.MethodGet, registry.TokenPath+"?"+query.Encode(), nil)
	req.SetBasicAuth(username, password)
	resp := httptest.NewRecorder()
	auth.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("token exchange status = %d: %s", resp.Code, resp.Body.String())
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if body.Token == "" {
		t.Fatal("token exchange returned an empty token")
	}
	return body.Token
}

func exchangeRegistryTokenExpect(t *testing.T, auth *registry.Auth, username, password, service string, want int) {
	t.Helper()
	query := url.Values{"service": {service}}
	req := httptest.NewRequest(http.MethodGet, registry.TokenPath+"?"+query.Encode(), nil)
	req.SetBasicAuth(username, password)
	resp := httptest.NewRecorder()
	auth.ServeHTTP(resp, req)
	if resp.Code != want {
		t.Fatalf("token exchange status = %d, want %d: %s", resp.Code, want, resp.Body.String())
	}
}

func rotationHello(agentID, sessionID, clusterID string) *agentv1.AgentHello {
	return &agentv1.AgentHello{
		AgentId: agentID, Name: "rotation agent",
		AdvertiseAddr:      "fd00:30::10",
		WireguardPublicKey: "rotation-public-key", WireguardListenPort: 51820,
		WireguardEndpoint: "[fd00:30::10]:51820",
		CpuMillisCapacity: 2000, MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities: []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:     "test",
		SessionId:           sessionID, ClusterId: clusterID,
		LocalStoreId: "rotation-store", InitializationState: "ready",
	}
}
