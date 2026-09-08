package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	identitycore "ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/testutil"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

const (
	productE2EUserID      = "dev-user"
	productE2EUserEmail   = "dev@example.com"
	productE2EServiceName = "fixture-web"
	productE2EImage       = "hashicorp/http-echo:1.0.0"
	productE2EPort        = int32(5678)
	productE2EMarkerV1    = "local-product-v1"
	productE2EMarkerV2    = "local-product-v2"

	// Must match console/src/lib/dashboard/core/jwt.server.ts.
	dashboardJWTIssuer   = "managed-dashboard"
	dashboardJWTAudience = "managed-dashboard"
	dashboardAccessTTL   = time.Hour
)

type productE2ESummary struct {
	ProjectID          string `json:"project_id"`
	ServiceID          string `json:"service_id"`
	ServiceName        string `json:"service_name"`
	RouteURL           string `json:"route_url"`
	Marker             string `json:"marker"`
	SessionCookieName  string `json:"session_cookie_name,omitempty"`
	SessionCookieValue string `json:"session_cookie_value,omitempty"`
}

func runProductE2EScenario(
	ctx context.Context,
	controlPlaneAddr string,
	identity identitycore.ClientIdentityMaterial,
	assertionSecret string,
	ingressPort int,
	publicBaseURL string,
	sessionCookieName string,
	dashboardJWTSecret string,
) (productE2ESummary, error) {
	if strings.TrimSpace(publicBaseURL) == "" {
		return productE2ESummary{}, fmt.Errorf("public base URL is required for product e2e")
	}
	clientCert, err := tls.X509KeyPair(identity.CertPEM, identity.KeyPEM)
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("parse dashboard identity: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(identity.CAPEM) {
		return productE2ESummary{}, fmt.Errorf("parse control-plane CA")
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()
	conn, err := grpc.DialContext(dialCtx, controlPlaneAddr,
		grpc.WithBlock(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{clientCert},
			RootCAs:      roots,
			ServerName:   "localhost",
			MinVersion:   tls.VersionTLS13,
		})),
	)
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("dial control plane: %w", err)
	}
	defer conn.Close()
	client := platformv1.NewPlatformServiceClient(conn)

	userCtx, err := productE2EUserContext(ctx, assertionSecret)
	if err != nil {
		return productE2ESummary{}, err
	}
	project, err := client.CreateProject(userCtx, &platformv1.CreateProjectRequest{Name: "local-product-e2e"})
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("create fixture project: %w", err)
	}
	environments, err := client.ListEnvironments(userCtx, &platformv1.ListEnvironmentsRequest{ProjectId: project.GetId()})
	if err != nil || len(environments.GetEnvironments()) != 1 {
		return productE2ESummary{}, fmt.Errorf("load production environment: %w", err)
	}
	environmentID := environments.GetEnvironments()[0].GetId()
	service, err := client.CreateService(userCtx, &platformv1.CreateServiceRequest{
		EnvironmentId: environmentID,
		Service: &platformv1.ServiceInput{
			Name: productE2EServiceName,
			Spec: productE2EServiceSpec(productE2EMarkerV1),
		},
	})
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("create fixture service: %w", err)
	}
	deployed, err := client.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID})
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("deploy production environment: %w", err)
	}
	if len(deployed.GetServices()) != 1 {
		return productE2ESummary{}, fmt.Errorf("deploy production environment: expected 1 service, got %d", len(deployed.GetServices()))
	}
	if _, err := waitForProductService(ctx, client, assertionSecret, service.GetId(), service.GetSpecRevision(), 1); err != nil {
		return productE2ESummary{}, fmt.Errorf("wait for initial deploy: %w", err)
	}

	userCtx, err = productE2EUserContext(ctx, assertionSecret)
	if err != nil {
		return productE2ESummary{}, err
	}
	binding, err := client.GenerateDomainBinding(userCtx, &platformv1.GenerateDomainBindingRequest{
		ServiceId:  service.GetId(),
		TargetPort: productE2EPort,
	})
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("generate fixture domain: %w", err)
	}
	routeURL, err := productRouteURL(binding.GetHostname(), ingressPort, publicBaseURL)
	if err != nil {
		return productE2ESummary{}, err
	}
	if err := waitForProductRoute(ctx, routeURL, productE2EMarkerV1); err != nil {
		return productE2ESummary{}, fmt.Errorf("verify initial domain route via public tunnel: %w", err)
	}

	userCtx, err = productE2EUserContext(ctx, assertionSecret)
	if err != nil {
		return productE2ESummary{}, err
	}
	updated, err := client.UpdateService(userCtx, &platformv1.UpdateServiceRequest{
		ServiceId: service.GetId(),
		Service: &platformv1.ServiceUpdate{
			Name: productE2EServiceName,
			Spec: productE2EServiceSpec(productE2EMarkerV2),
		},
	})
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("release staged fixture: %w", err)
	}
	if err := waitForProductRoute(ctx, routeURL, productE2EMarkerV1); err != nil {
		return productE2ESummary{}, fmt.Errorf("draft changed live route before release: %w", err)
	}
	userCtx, err = productE2EUserContext(ctx, assertionSecret)
	if err != nil {
		return productE2ESummary{}, err
	}
	release, err := client.ReleaseEnvironment(userCtx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: environmentID})
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("deploy updated fixture service: %w", err)
	}
	if len(release.GetServices()) != 1 {
		return productE2ESummary{}, fmt.Errorf("deploy updated fixture service: expected 1 service, got %d", len(release.GetServices()))
	}
	released := release.GetServices()[0]
	healthy, err := waitForProductService(ctx, client, assertionSecret, service.GetId(), updated.GetSpecRevision(), released.GetService().GetRolloutGeneration())
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("wait for fixture release: %w", err)
	}
	deploymentHistory, err := client.ListServiceDeployments(userCtx, &platformv1.ListServiceDeploymentsRequest{ServiceId: service.GetId(), Limit: 1})
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("list fixture deployment for restart: %w", err)
	}
	if len(deploymentHistory.GetDeployments()) != 1 {
		return productE2ESummary{}, fmt.Errorf("list fixture deployment for restart: got %d deployments", len(deploymentHistory.GetDeployments()))
	}
	restartAllocation := matchingProductAllocation(healthy, updated.GetSpecRevision(), released.GetService().GetRolloutGeneration())
	if restartAllocation == nil {
		return productE2ESummary{}, fmt.Errorf("released service has no matching healthy allocation to restart")
	}
	restartRequest := &platformv1.ApplyDeploymentActionRequest{
		ServiceId:      service.GetId(),
		DeploymentId:   deploymentHistory.GetDeployments()[0].GetId(),
		Action:         platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART,
		IdempotencyKey: uuid.NewString(),
		AllocationId:   healthy.GetAllocation().GetAllocationId(),
	}
	if _, err := client.ApplyDeploymentAction(userCtx, restartRequest); err != nil {
		return productE2ESummary{}, fmt.Errorf("restart fixture allocation: %w", err)
	}
	if _, err := client.ApplyDeploymentAction(userCtx, restartRequest); err != nil {
		return productE2ESummary{}, fmt.Errorf("replay fixture restart: %w", err)
	}
	if _, err := waitForProductService(ctx, client, assertionSecret, service.GetId(), updated.GetSpecRevision(), released.GetService().GetRolloutGeneration()); err != nil {
		return productE2ESummary{}, fmt.Errorf("wait for fixture restart: %w", err)
	}
	if err := waitForProductRoute(ctx, routeURL, productE2EMarkerV2); err != nil {
		return productE2ESummary{}, fmt.Errorf("verify released domain route via public tunnel: %w", err)
	}

	sessionToken, err := mintDashboardAccessToken(dashboardJWTSecret, productE2EUserID, productE2EUserEmail)
	if err != nil {
		return productE2ESummary{}, fmt.Errorf("mint dashboard session for e2e: %w", err)
	}

	return productE2ESummary{
		ProjectID:          project.GetId(),
		ServiceID:          service.GetId(),
		ServiceName:        productE2EServiceName,
		RouteURL:           routeURL,
		Marker:             productE2EMarkerV2,
		SessionCookieName:  sessionCookieName,
		SessionCookieValue: sessionToken,
	}, nil
}

// productRouteURL builds the externally reachable service URL. Public tunnel
// mode uses HTTPS on the generated platform hostname (no local ingress port).
func productRouteURL(hostname string, ingressPort int, publicBaseURL string) (string, error) {
	hostname = strings.TrimSpace(strings.ToLower(hostname))
	if hostname == "" {
		return "", fmt.Errorf("generated hostname is empty")
	}
	if strings.TrimSpace(publicBaseURL) == "" {
		return fmt.Sprintf("http://%s:%d/", hostname, ingressPort), nil
	}
	parsed, err := url.Parse(strings.TrimSpace(publicBaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid public base URL %q", publicBaseURL)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("public product routes require https, got %q", parsed.Scheme)
	}
	return "https://" + hostname + "/", nil
}

// mintDashboardAccessToken creates a short-lived access cookie value that only
// works with this run's DASHBOARD_JWT_SECRET. It is written to a local artifact
// for Playwright and is never exposed as an open login method on the public UI.
func mintDashboardAccessToken(secret, userID, email string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("dashboard JWT secret is required")
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   dashboardJWTIssuer,
		"aud":   dashboardJWTAudience,
		"typ":   "access",
		"sub":   userID,
		"email": email,
		"iat":   now.Unix(),
		"exp":   now.Add(dashboardAccessTTL).Unix(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		return "", err
	}
	return token, nil
}

// seedProductE2EDashboardUser applies the dashboard v1 schema (if needed) and
// inserts the fixture owner so a minted access cookie can load home without
// open public dev logins. Must stay aligned with console dashboardStoreMigrations.
func seedProductE2EDashboardUser(ctx context.Context, databaseURL, schema, userID, email string) error {
	schema = strings.TrimSpace(schema)
	userID = strings.TrimSpace(userID)
	email = strings.TrimSpace(email)
	if schema == "" || userID == "" || email == "" {
		return fmt.Errorf("dashboard schema, user id, and email are required")
	}
	if !isSafeSQLIdent(schema) {
		return fmt.Errorf("unsafe dashboard schema %q", schema)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open dashboard database: %w", err)
	}
	defer db.Close()

	users := schema + ".users"
	accounts := schema + ".accounts"
	onboarding := schema + ".onboarding"
	migrations := schema + ".schema_migrations"
	sessions := schema + ".sessions"
	refreshSessions := schema + ".refresh_sessions"
	servicePositions := schema + ".service_positions"

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dashboard seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema); err != nil {
		return fmt.Errorf("create dashboard schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+migrations+` (
			version INT8 PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+migrations+` WHERE version = 1`).Scan(&applied); err != nil {
		return fmt.Errorf("check dashboard migrations: %w", err)
	}
	if applied == 0 {
		for _, statement := range []string{
			`CREATE TABLE ` + users + ` (
				id STRING PRIMARY KEY,
				email STRING NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE ` + sessions + ` (
				id STRING PRIMARY KEY,
				user_id STRING NOT NULL REFERENCES ` + users + `(id) ON DELETE CASCADE,
				created_at TIMESTAMPTZ NOT NULL,
				expires_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX ` + schema + `_sessions_expires_at_idx ON ` + sessions + ` (expires_at)`,
			`CREATE TABLE ` + refreshSessions + ` (
				id STRING PRIMARY KEY,
				user_id STRING NOT NULL REFERENCES ` + users + `(id) ON DELETE CASCADE,
				created_at TIMESTAMPTZ NOT NULL,
				expires_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX ` + schema + `_refresh_sessions_expires_at_idx ON ` + refreshSessions + ` (expires_at)`,
			`CREATE TABLE ` + accounts + ` (
				id STRING PRIMARY KEY,
				user_id STRING NOT NULL REFERENCES ` + users + `(id) ON DELETE CASCADE,
				provider STRING NOT NULL,
				provider_subject STRING NOT NULL,
				verified_email_snapshot STRING NOT NULL DEFAULT '',
				provider_login STRING NOT NULL DEFAULT '',
				access_token STRING NOT NULL DEFAULT '',
				access_token_expires_at TIMESTAMPTZ NULL,
				refresh_token STRING NOT NULL DEFAULT '',
				refresh_token_expires_at TIMESTAMPTZ NULL,
				token_type STRING NOT NULL DEFAULT '',
				scope STRING NOT NULL DEFAULT '',
				oauth_token_version INT8 NOT NULL DEFAULT 0,
				oauth_refresh_lease_id STRING NULL,
				oauth_refresh_lease_expires_at TIMESTAMPTZ NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				last_login_at TIMESTAMPTZ NOT NULL,
				UNIQUE(provider, provider_subject)
			)`,
			`CREATE INDEX ` + schema + `_accounts_provider_email_idx ON ` + accounts + ` (provider, lower(verified_email_snapshot))`,
			`CREATE TABLE ` + onboarding + ` (
				user_id STRING PRIMARY KEY REFERENCES ` + users + `(id) ON DELETE CASCADE,
				account_name STRING NOT NULL DEFAULT '',
				status STRING NOT NULL DEFAULT 'pending',
				current_step STRING NOT NULL DEFAULT 'account',
				project_id STRING NOT NULL DEFAULT '',
				environment_id STRING NOT NULL DEFAULT '',
				service_id STRING NOT NULL DEFAULT '',
				repository_selector STRING NOT NULL DEFAULT '',
				tracked_ref STRING NOT NULL DEFAULT '',
				dockerfile_path STRING NOT NULL DEFAULT '',
				context_dir STRING NOT NULL DEFAULT '',
				hostname STRING NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE ` + servicePositions + ` (
				user_id STRING NOT NULL REFERENCES ` + users + `(id) ON DELETE CASCADE,
				environment_id STRING NOT NULL,
				service_id STRING NOT NULL,
				x INT8 NOT NULL,
				y INT8 NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (user_id, environment_id, service_id)
			)`,
			`CREATE INDEX ` + schema + `_service_positions_environment_idx ON ` + servicePositions + ` (user_id, environment_id)`,
			`INSERT INTO ` + migrations + ` (version, applied_at) VALUES (1, NOW())`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply dashboard migration: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+users+` (id, email, created_at, updated_at)
		VALUES ($1, $2, NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET email = excluded.email, updated_at = NOW()`,
		userID, email,
	); err != nil {
		return fmt.Errorf("seed dashboard user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+accounts+` (
			id, user_id, provider, provider_subject, verified_email_snapshot, provider_login,
			created_at, updated_at, last_login_at
		) VALUES ($1, $2, 'dev', $2, $3, $2, NOW(), NOW(), NOW())
		ON CONFLICT (provider, provider_subject) DO UPDATE
		   SET verified_email_snapshot = excluded.verified_email_snapshot,
		       provider_login = excluded.provider_login,
		       updated_at = NOW(),
		       last_login_at = NOW()`,
		uuid.NewString(), userID, email,
	); err != nil {
		return fmt.Errorf("seed dashboard account: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+onboarding+` (user_id, created_at, updated_at)
		VALUES ($1, NOW(), NOW())
		ON CONFLICT (user_id) DO NOTHING`,
		userID,
	); err != nil {
		return fmt.Errorf("seed dashboard onboarding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dashboard seed: %w", err)
	}
	return nil
}

func isSafeSQLIdent(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func productE2EServiceSpec(marker string) *platformv1.ServiceSpec {
	return &platformv1.ServiceSpec{
		Source: &platformv1.ServiceSource{Source: &platformv1.ServiceSource_Image{Image: &platformv1.DirectImageSource{Image: productE2EImage}}},
		Runtime: &platformv1.ServiceRuntime{
			Args:            []string{"-listen=:5678", "-text=" + marker},
			CpuMillis:       250,
			MemoryMebibytes: 256,
			Ports:           []*platformv1.ServiceRuntimePort{{Port: productE2EPort, Primary: true}},
			HealthCheck: &platformv1.HealthCheck{
				Type:           platformv1.HealthCheck_TYPE_HTTP,
				Path:           "/",
				Port:           productE2EPort,
				TimeoutSeconds: 1,
			},
		},
	}
}

func productE2EUserContext(ctx context.Context, secret string) (context.Context, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    "managed-dashboard",
		Audience:  jwt.ClaimStrings{"controlplane"},
		Subject:   productE2EUserID,
		ExpiresAt: jwt.NewNumericDate(now.Add(30 * time.Second)),
		IssuedAt:  jwt.NewNumericDate(now),
		ID:        fmt.Sprintf("local-product-%d", now.UnixNano()),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("sign fixture user assertion: %w", err)
	}
	return metadata.AppendToOutgoingContext(ctx, "x-platform-user-assertion", token), nil
}

func waitForProductService(ctx context.Context, client platformv1.PlatformServiceClient, assertionSecret, serviceID string, specRevision, rolloutGeneration int64) (*platformv1.ServiceStatus, error) {
	var latest *platformv1.ServiceStatus
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 90 * time.Second, Interval: 250 * time.Millisecond}, func(context.Context) (bool, error) {
		userCtx, err := productE2EUserContext(ctx, assertionSecret)
		if err != nil {
			return false, err
		}
		requestCtx, cancel := context.WithTimeout(userCtx, 5*time.Second)
		defer cancel()
		status, err := client.GetServiceStatus(requestCtx, &platformv1.GetServiceStatusRequest{ServiceId: serviceID})
		if err != nil {
			return false, nil
		}
		latest = status
		allocation := status.GetAllocation()
		return allocation.GetHealthy() &&
			allocation.GetAppliedSpecRevision() >= specRevision &&
			allocation.GetAppliedRolloutGeneration() >= rolloutGeneration, nil
	})
	if err != nil {
		return latest, err
	}
	return latest, nil
}

func matchingProductAllocation(status *platformv1.ServiceStatus, specRevision, rolloutGeneration int64) *platformv1.AllocationStatus {
	if status.GetService().GetLatestDeployment().GetState() != platformv1.DeploymentState_DEPLOYMENT_STATE_ACTIVE {
		return nil
	}
	for _, allocation := range status.GetAllocations() {
		if allocation.GetHealthy() &&
			allocation.GetRolloutState() == "serving" &&
			allocation.GetAppliedSpecRevision() >= specRevision &&
			allocation.GetAppliedRolloutGeneration() >= rolloutGeneration {
			return allocation
		}
	}
	return nil
}

func waitForProductRoute(ctx context.Context, routeURL, marker string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout: 3 * time.Second,
			}).DialContext,
		},
	}
	// Public tunnel + DNS propagation can take longer than local ingress.
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 90 * time.Second, Interval: 500 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, routeURL, nil)
		if err != nil {
			return false, err
		}
		response, err := client.Do(request)
		if err != nil {
			return false, nil
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			return false, nil
		}
		return response.StatusCode == http.StatusOK && strings.Contains(string(body), marker), nil
	})
}
