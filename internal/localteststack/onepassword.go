package localteststack

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"ebof-wg-mesh/internal/config"
	onepassword "github.com/1password/onepassword-sdk-go"
)

const (
	OPEnvironmentIDKey       = "OP_ENVIRONMENT_ID"
	OPServiceAccountTokenKey = "OP_SERVICE_ACCOUNT_TOKEN"
	OPAccountKey             = "OP_ACCOUNT"

	ControlPlaneGitHubAppIDKey            = "CONTROLPLANE_GITHUB_APP_ID"
	ControlPlaneGitHubWebhookSecretKey    = "CONTROLPLANE_GITHUB_WEBHOOK_SECRET"
	ControlPlaneGitHubPrivateKeyPEMKey    = "CONTROLPLANE_GITHUB_PRIVATE_KEY_PEM"
	ControlPlaneDashboardGitHubInstallKey = "CONTROLPLANE_DASHBOARD_GITHUB_INSTALL_URL"
	ControlPlaneGitHubAPIBaseURLKey       = "CONTROLPLANE_GITHUB_API_BASE_URL"
	ControlPlaneGitHubWebhookPathKey      = "CONTROLPLANE_GITHUB_WEBHOOK_PATH"
	DashboardGitHubAppIDKey               = "DASHBOARD_GITHUB_APP_ID"
	DashboardGitHubClientIDKey            = "DASHBOARD_GITHUB_CLIENT_ID"
	DashboardGitHubClientSecretKey        = "DASHBOARD_GITHUB_CLIENT_SECRET"
	DashboardGitHubAuthBaseURLKey         = "DASHBOARD_GITHUB_AUTH_BASE_URL"
	DashboardGitHubAPIBaseURLKey          = "DASHBOARD_GITHUB_API_BASE_URL"
	DashboardGitHubInstallURLKey          = "DASHBOARD_GITHUB_INSTALL_URL"

	DashboardPublicBaseURLKey     = "DASHBOARD_PUBLIC_BASE_URL"
	DashboardIngressTargetHostKey = "DASHBOARD_INGRESS_TARGET_HOST"

	defaultGitHubWebhookPath = "/webhooks/github"
)

type AuthMethod string

const (
	AuthMethodNone           AuthMethod = ""
	AuthMethodServiceAccount AuthMethod = "service_account"
	AuthMethodDesktopApp     AuthMethod = "desktop_app"
)

type EnvironmentLoaderConfig struct {
	EnvironmentID       string
	ServiceAccountToken string
	Account             string
	IntegrationName     string
	IntegrationVersion  string
}

type SelectedAuth struct {
	Method              AuthMethod
	ServiceAccountToken string
	Account             string
}

func (a SelectedAuth) Configured() bool {
	return a.Method != AuthMethodNone
}

type ClientFactory func(context.Context, ...onepassword.ClientOption) (*onepassword.Client, error)

type EnvironmentLoader struct {
	cfg       EnvironmentLoaderConfig
	newClient ClientFactory

	clientOnce sync.Once
	client     *onepassword.Client
	clientErr  error
}

type OverlayResult struct {
	PublicBaseURL      string
	GitHubEnabled      bool
	GitHubCallbackURL  string
	GitHubWebhookURL   string
	MissingGitHubKeys  []string
	MissingRuntimeKeys []string
}

func EnvironmentLoaderConfigFromLookup(lookup func(string) string) EnvironmentLoaderConfig {
	return EnvironmentLoaderConfig{
		EnvironmentID:       strings.TrimSpace(lookup(OPEnvironmentIDKey)),
		ServiceAccountToken: strings.TrimSpace(lookup(OPServiceAccountTokenKey)),
		Account:             strings.TrimSpace(lookup(OPAccountKey)),
		IntegrationName:     "ebpf-wg-mesh",
		IntegrationVersion:  "localteststack",
	}
}

func OverlayKeys() []string {
	return []string{
		ControlPlaneGitHubAppIDKey,
		ControlPlaneGitHubWebhookSecretKey,
		ControlPlaneGitHubPrivateKeyPEMKey,
		ControlPlaneDashboardGitHubInstallKey,
		ControlPlaneGitHubAPIBaseURLKey,
		ControlPlaneGitHubWebhookPathKey,
		DashboardGitHubAppIDKey,
		DashboardGitHubClientIDKey,
		DashboardGitHubClientSecretKey,
		DashboardGitHubAuthBaseURLKey,
		DashboardGitHubAPIBaseURLKey,
		DashboardGitHubInstallURLKey,
		CloudflareTunnelTokenKey,
		CloudflareHostnameKey,
	}
}

func OverlayEnvFromLookup(lookup func(string) string) map[string]string {
	if lookup == nil {
		return nil
	}
	env := make(map[string]string)
	for _, key := range OverlayKeys() {
		if value := strings.TrimSpace(lookup(key)); value != "" {
			env[key] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

func MergeOverlayEnv(base, primary map[string]string) map[string]string {
	if len(base) == 0 && len(primary) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(primary))
	for key, value := range base {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	for key, value := range primary {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func SelectAuth(cfg EnvironmentLoaderConfig) SelectedAuth {
	if token := strings.TrimSpace(cfg.ServiceAccountToken); token != "" {
		return SelectedAuth{
			Method:              AuthMethodServiceAccount,
			ServiceAccountToken: token,
		}
	}
	if account := strings.TrimSpace(cfg.Account); account != "" {
		return SelectedAuth{
			Method:  AuthMethodDesktopApp,
			Account: account,
		}
	}
	return SelectedAuth{}
}

func NewEnvironmentLoader(cfg EnvironmentLoaderConfig) *EnvironmentLoader {
	return NewEnvironmentLoaderWithFactory(cfg, onepassword.NewClient)
}

func NewEnvironmentLoaderWithFactory(cfg EnvironmentLoaderConfig, factory ClientFactory) *EnvironmentLoader {
	if cfg.IntegrationName == "" {
		cfg.IntegrationName = "ebpf-wg-mesh"
	}
	if cfg.IntegrationVersion == "" {
		cfg.IntegrationVersion = "localteststack"
	}
	return &EnvironmentLoader{
		cfg:       cfg,
		newClient: factory,
	}
}

func (l *EnvironmentLoader) Enabled() bool {
	return strings.TrimSpace(l.cfg.EnvironmentID) != ""
}

func (l *EnvironmentLoader) Auth() SelectedAuth {
	return SelectAuth(l.cfg)
}

func (l *EnvironmentLoader) Ready() bool {
	return l.Enabled() && l.Auth().Configured()
}

func (l *EnvironmentLoader) Load(ctx context.Context) (map[string]string, error) {
	if !l.Ready() {
		return nil, nil
	}
	client, err := l.clientForLoad(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize 1Password client: %w", err)
	}
	environmentID := strings.TrimSpace(l.cfg.EnvironmentID)
	response, err := client.Environments().GetVariables(ctx, environmentID)
	if err != nil {
		return nil, fmt.Errorf("read 1Password environment %s: %w", environmentID, err)
	}
	vars, err := variablesToMap(response.Variables)
	if err != nil {
		return nil, fmt.Errorf("normalize 1Password environment %s: %w", environmentID, err)
	}
	return vars, nil
}

func (l *EnvironmentLoader) clientForLoad(ctx context.Context) (*onepassword.Client, error) {
	l.clientOnce.Do(func() {
		auth := l.Auth()
		opts := []onepassword.ClientOption{
			onepassword.WithIntegrationInfo(l.cfg.IntegrationName, l.cfg.IntegrationVersion),
		}
		switch auth.Method {
		case AuthMethodServiceAccount:
			opts = append(opts, onepassword.WithServiceAccountToken(auth.ServiceAccountToken))
		case AuthMethodDesktopApp:
			opts = append(opts, onepassword.WithDesktopAppIntegration(auth.Account))
		default:
			l.clientErr = fmt.Errorf("no 1Password auth configured")
			return
		}
		l.client, l.clientErr = l.newClient(ctx, opts...)
	})
	return l.client, l.clientErr
}

func variablesToMap(variables []onepassword.EnvironmentVariable) (map[string]string, error) {
	env := make(map[string]string, len(variables))
	for _, variable := range variables {
		key, err := normalizeVariableKey(variable.Name)
		if err != nil {
			return nil, err
		}
		if _, exists := env[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		env[key] = variable.Value
	}
	return env, nil
}

func normalizeVariableKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("blank variable name")
	}
	return key, nil
}

func ApplyEnvironmentOverlay(cfg *config.ControlPlaneConfig, dashboardEnv map[string]string, env map[string]string, publicBaseURL string) (OverlayResult, error) {
	var result OverlayResult
	if cfg == nil {
		return result, fmt.Errorf("control plane config is required")
	}
	if dashboardEnv == nil {
		return result, fmt.Errorf("dashboard env is required")
	}
	webhookPath := effectiveGitHubWebhookPath(env)

	if publicBaseURL != "" {
		parsed, err := parseHTTPSBaseURL(publicBaseURL)
		if err != nil {
			return result, fmt.Errorf("invalid public base url %q: %w", publicBaseURL, err)
		}
		cfg.Ingress.PublicAddr = parsed.Host
		dashboardEnv[DashboardPublicBaseURLKey] = parsed.String()
		if strings.TrimSpace(dashboardEnv[DashboardIngressTargetHostKey]) == "" {
			dashboardEnv[DashboardIngressTargetHostKey] = parsed.Host
		}
		result.PublicBaseURL = parsed.String()
		result.GitHubCallbackURL = deriveURL(result.PublicBaseURL, "/auth/callback")
		result.GitHubWebhookURL = deriveURL(result.PublicBaseURL, effectiveGitHubWebhookPath(env))
	}

	missingKeys := MissingGitHubKeys(env)
	if len(missingKeys) > 0 {
		result.MissingGitHubKeys = missingKeys
		return result, nil
	}

	if result.PublicBaseURL == "" {
		result.MissingRuntimeKeys = []string{CloudflareTunnelTokenKey, CloudflareHostnameKey}
		return result, nil
	}

	appID, err := parseGitHubAppID(env[ControlPlaneGitHubAppIDKey])
	if err != nil {
		return result, err
	}
	cfg.GitHub.Enabled = true
	cfg.GitHub.AppID = appID
	cfg.GitHub.WebhookSecret = strings.TrimSpace(env[ControlPlaneGitHubWebhookSecretKey])
	privateKeyPEM, err := decodeGitHubPrivateKeyPEM(env[ControlPlaneGitHubPrivateKeyPEMKey])
	if err != nil {
		return result, err
	}
	cfg.GitHub.PrivateKeyPEM = privateKeyPEM
	cfg.GitHub.APIBaseURL = optionalEnv(env, ControlPlaneGitHubAPIBaseURLKey)
	cfg.GitHub.WebhookPath = webhookPath

	if installURL := strings.TrimSpace(env[ControlPlaneDashboardGitHubInstallKey]); installURL != "" {
		dashboardEnv[DashboardGitHubInstallURLKey] = installURL
	}
	dashboardEnv[DashboardGitHubAppIDKey] = strings.TrimSpace(env[DashboardGitHubAppIDKey])
	dashboardEnv[DashboardGitHubClientIDKey] = strings.TrimSpace(env[DashboardGitHubClientIDKey])
	dashboardEnv[DashboardGitHubClientSecretKey] = env[DashboardGitHubClientSecretKey]
	if authBaseURL := optionalEnv(env, DashboardGitHubAuthBaseURLKey); authBaseURL != "" {
		dashboardEnv[DashboardGitHubAuthBaseURLKey] = authBaseURL
	}
	if apiBaseURL := optionalEnv(env, DashboardGitHubAPIBaseURLKey); apiBaseURL != "" {
		dashboardEnv[DashboardGitHubAPIBaseURLKey] = apiBaseURL
	}

	result.GitHubEnabled = true
	return result, nil
}

func MissingGitHubKeys(env map[string]string) []string {
	missing := make([]string, 0, 12)
	for _, key := range []string{
		ControlPlaneGitHubAppIDKey,
		ControlPlaneGitHubWebhookSecretKey,
		ControlPlaneGitHubPrivateKeyPEMKey,
		DashboardGitHubAppIDKey,
		DashboardGitHubClientIDKey,
		DashboardGitHubClientSecretKey,
	} {
		if strings.TrimSpace(env[key]) == "" {
			missing = append(missing, key)
		}
	}
	slices.Sort(missing)
	return missing
}

func parseHTTPSBaseURL(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, fmt.Errorf("value is empty")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if !parsed.IsAbs() {
		return nil, fmt.Errorf("must be absolute")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("must use https")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return nil, fmt.Errorf("host is required")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("userinfo is not allowed")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("query and fragment are not allowed")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, fmt.Errorf("path is not allowed")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed, nil
}

func deriveURL(baseURL string, path string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	ref := &url.URL{Path: path}
	return base.ResolveReference(ref).String()
}

func decodeGitHubPrivateKeyPEM(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("%s is required", ControlPlaneGitHubPrivateKeyPEMKey)
	}
	if strings.Contains(value, "-----BEGIN ") {
		return value, nil
	}
	compact := strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "").Replace(value)
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return "", fmt.Errorf(
			"%s must be raw PEM or base64-encoded PEM: %w",
			ControlPlaneGitHubPrivateKeyPEMKey,
			err,
		)
	}
	decodedValue := strings.TrimSpace(string(decoded))
	if !strings.Contains(decodedValue, "-----BEGIN ") {
		return "", fmt.Errorf(
			"%s decoded from base64 but did not look like PEM",
			ControlPlaneGitHubPrivateKeyPEMKey,
		)
	}
	return decodedValue, nil
}

func effectiveGitHubWebhookPath(env map[string]string) string {
	if path := optionalEnv(env, ControlPlaneGitHubWebhookPathKey); path != "" {
		return path
	}
	return defaultGitHubWebhookPath
}

func parseGitHubAppID(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	appID, err := strconv.ParseInt(value, 10, 64)
	if err != nil || appID <= 0 {
		return 0, fmt.Errorf("invalid %s value %q", ControlPlaneGitHubAppIDKey, raw)
	}
	return appID, nil
}

func optionalEnv(env map[string]string, key string) string {
	return strings.TrimSpace(env[key])
}

func firstNonEmpty(env map[string]string, keys ...string) (string, string, bool) {
	for _, key := range keys {
		if value := strings.TrimSpace(env[key]); value != "" {
			return value, key, true
		}
	}
	return "", "", false
}
