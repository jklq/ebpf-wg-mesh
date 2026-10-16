package productionops

import (
	"embed"
	"fmt"
	"net"
	"sort"
	"strings"

	"ebof-wg-mesh/internal/deploy"
)

// Observability ports. All listeners bind the host's private address
// (Network.SocketHost), never 0.0.0.0: the fleet host network is the
// trust boundary, and the scrape token is defense in depth on top of it.
const (
	metricsPort         = "9095" // Go components (TELEMETRY_METRICS_LISTEN)
	prometheusPort      = "9099"
	grafanaPort         = "3001"
	tempoHTTPPort       = "3200"
	tempoOTLPGRPCPort   = "4327"
	collectorGRPCPort   = "4317"
	collectorHTTPPort   = "4318"
	collectorHealthPort = "13133"
	collectorMetricPort = "8888"
)

//go:embed assets/dashboards/*.json assets/rules/*.yaml
var observabilityAssets embed.FS

// observabilityTopology resolves placement addresses once per credentials
// run so every rendered file agrees on endpoints.
type observabilityTopology struct {
	plan       deploy.Plan
	byRole     map[deploy.Role][]deploy.Placement
	token      string
	hasObs     bool
	tempoAddr  string // host:port of first Tempo placement (OTLP gRPC)
	tempoHTTP  string
	promAddr   string
	collectors []string // host:4317 endpoints
}

func (r *Runner) observability(token string) observabilityTopology {
	t := observabilityTopology{plan: r.Plan, byRole: map[deploy.Role][]deploy.Placement{}, token: token}
	for _, pl := range r.Plan.Placements {
		t.byRole[pl.Role] = append(t.byRole[pl.Role], pl)
	}
	for _, list := range t.byRole {
		sort.Slice(list, func(a, b int) bool { return list[a].Instance < list[b].Instance })
	}
	for _, role := range deploy.ObservabilityRoles {
		if len(t.byRole[role]) > 0 {
			t.hasObs = true
		}
	}
	if list := t.byRole[deploy.Tempo]; len(list) > 0 {
		t.tempoAddr = t.addr(list[0], tempoOTLPGRPCPort)
		t.tempoHTTP = "http://" + t.addr(list[0], tempoHTTPPort)
	}
	if list := t.byRole[deploy.Prometheus]; len(list) > 0 {
		t.promAddr = t.addr(list[0], prometheusPort)
	}
	for _, pl := range t.byRole[deploy.OTelCollector] {
		t.collectors = append(t.collectors, t.addr(pl, collectorGRPCPort))
	}
	sort.Strings(t.collectors)
	return t
}

func (t observabilityTopology) addr(pl deploy.Placement, port string) string {
	h, _ := t.plan.Installation.Host(pl.Host)
	return net.JoinHostPort(h.Network.Address, port)
}

func (t observabilityTopology) socketHost(pl deploy.Placement) string {
	h, _ := t.plan.Installation.Host(pl.Host)
	return h.Network.SocketHost()
}

// telemetryEnv is injected into every Go component's runtime environment:
// a dedicated scrape listener on the private host address, OTLP export to
// the first collector (agents/builders reconnect logic does not apply to
// telemetry; the exporter retries and drops rather than blocking work),
// and the shared scrape token when the operator configured one.
func (t observabilityTopology) telemetryEnv(pl deploy.Placement, env map[string]string) {
	switch pl.Role {
	case deploy.ControlPlane, deploy.Agent, deploy.Builder:
	default:
		return
	}
	h, _ := t.plan.Installation.Host(pl.Host)
	env["TELEMETRY_METRICS_LISTEN"] = net.JoinHostPort(h.Network.Address, metricsPort)
	if len(t.collectors) > 0 {
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = t.collectors[0]
		env["OTEL_EXPORTER_OTLP_INSECURE"] = "true"
	}
	if t.token != "" {
		env["TELEMETRY_METRICS_TOKEN"] = t.token
	}
}

// render returns the config files for one observability placement, keyed
// by path relative to its configuration directory.
func (t observabilityTopology) render(pl deploy.Placement) (map[string][]byte, error) {
	switch pl.Role {
	case deploy.Prometheus:
		return t.renderPrometheus(pl)
	case deploy.Grafana:
		return t.renderGrafana(pl)
	case deploy.Tempo:
		return t.renderTempo(pl), nil
	case deploy.OTelCollector:
		return t.renderCollector(pl), nil
	default:
		return nil, nil
	}
}

func (t observabilityTopology) renderPrometheus(pl deploy.Placement) (map[string][]byte, error) {
	cfg := cfgDir(t.plan, pl)
	var b strings.Builder
	b.WriteString("# Rendered by operations from the applied plan. Do not edit on the host.\n")
	b.WriteString("global:\n  scrape_interval: 15s\n  scrape_timeout: 10s\n  evaluation_interval: 15s\n")
	fmt.Fprintf(&b, "  external_labels:\n    installation: %q\n", t.plan.Installation.ID)
	fmt.Fprintf(&b, "rule_files:\n  - %q\n", cfg+"/rules/*.yaml")
	b.WriteString("scrape_configs:\n")

	type target struct{ addr, instance, host string }
	job := func(name string, targets []target, metricsPath string, bearer bool, extra string) {
		if len(targets) == 0 {
			return
		}
		fmt.Fprintf(&b, "  - job_name: %q\n    metrics_path: %q\n    scheme: http\n", name, metricsPath)
		if bearer && t.token != "" {
			fmt.Fprintf(&b, "    authorization:\n      type: Bearer\n      credentials_file: %q\n", cfg+"/metrics-token")
		}
		// Drop empty OTel scope labels so series stay clean in queries.
		b.WriteString("    metric_relabel_configs:\n      - regex: \"otel_scope_(schema_url|version)\"\n        action: labeldrop\n")
		b.WriteString(extra)
		b.WriteString("    static_configs:\n")
		for _, tg := range targets {
			fmt.Fprintf(&b, "      - targets: [%q]\n        labels: {instance: %q, host: %q, role: %q}\n", tg.addr, tg.instance, tg.host, name)
		}
	}
	targetsFor := func(role deploy.Role, port string) []target {
		var out []target
		for _, p := range t.byRole[role] {
			out = append(out, target{addr: t.addr(p, port), instance: p.Instance, host: p.Host})
		}
		return out
	}
	// Exemplars carry trace IDs from OTel histograms; storage must accept
	// OpenMetrics (default scrape protocol negotiates it from promhttp).
	job("platform-controlplane", targetsFor(deploy.ControlPlane, metricsPort), "/metrics", true, "")
	job("platform-agent", targetsFor(deploy.Agent, metricsPort), "/metrics", true, "")
	job("platform-builder", targetsFor(deploy.Builder, metricsPort), "/metrics", true, "")
	job("prometheus", targetsFor(deploy.Prometheus, prometheusPort), "/metrics", false, "")
	job("tempo", targetsFor(deploy.Tempo, tempoHTTPPort), "/metrics", false, "")
	job("otel-collector", targetsFor(deploy.OTelCollector, collectorMetricPort), "/metrics", false, "")
	job("grafana", targetsFor(deploy.Grafana, grafanaPort), "/metrics", false, "")

	files := map[string][]byte{"prometheus.yaml": []byte(b.String())}
	if t.token != "" {
		files["metrics-token"] = []byte(t.token + "\n")
	}
	rules, err := observabilityAssets.ReadFile("assets/rules/platform.yaml")
	if err != nil {
		return nil, fmt.Errorf("embedded prometheus rules: %w", err)
	}
	files["rules/platform.yaml"] = rules
	return files, nil
}

func (t observabilityTopology) renderGrafana(pl deploy.Placement) (map[string][]byte, error) {
	cfg := cfgDir(t.plan, pl)
	data := dataDir(t.plan, pl)
	ini := fmt.Sprintf(`# Rendered by operations from the applied plan. Do not edit on the host.
app_mode = production
instance_name = %s

[paths]
data = %s/data
logs = %s/log
plugins = %s/plugins
provisioning = %s/provisioning

[server]
protocol = http
http_addr = %s
http_port = %s
root_url = %%(protocol)s://%%(domain)s:%s/
enable_gzip = true

[database]
type = sqlite3
path = %s/data/grafana.db

[security]
# The admin password arrives as GF_SECURITY_ADMIN_PASSWORD through the
# component secretEnv; it is never rendered into this file.
admin_user = admin
allow_embedding = false
cookie_secure = false
cookie_samesite = strict
disable_gravatar = true

[users]
allow_sign_up = false
default_theme = system

[auth.anonymous]
enabled = false

[analytics]
reporting_enabled = false
check_for_updates = false

[datasources]
default_datasource = Prometheus

[unified_alerting]
enabled = false

[log]
mode = console
level = info
`, pl.Instance, data, data, data, cfg, t.socketHost(pl), grafanaPort, grafanaPort, data)

	promURL := ""
	if t.promAddr != "" {
		promURL = "http://" + t.promAddr
	}
	datasources := fmt.Sprintf(`apiVersion: 1
deleteDatasources: []
datasources:
  - name: Prometheus
    uid: prometheus
    type: prometheus
    access: proxy
    url: %s
    isDefault: true
    editable: false
    jsonData:
      timeInterval: 15s
      exemplarTraceIdDestinations:
        - name: trace_id
          datasourceUid: tempo
          urlDisplayLabel: "View trace"
  - name: Tempo
    uid: tempo
    type: tempo
    access: proxy
    url: %s
    editable: false
    jsonData:
      serviceMap:
        datasourceUid: prometheus
      nodeGraph:
        enabled: true
`, promURL, t.tempoHTTP)

	dashProvider := fmt.Sprintf(`apiVersion: 1
providers:
  - name: Platform
    orgId: 1
    folder: Platform
    type: file
    disableDeletion: true
    updateIntervalSeconds: 60
    allowUiUpdates: false
    options:
      path: %s/dashboards
      foldersFromFilesStructure: false
`, cfg)

	files := map[string][]byte{
		"grafana.ini":                            []byte(ini),
		"provisioning/datasources/platform.yaml": []byte(datasources),
		"provisioning/dashboards/platform.yaml":  []byte(dashProvider),
	}
	entries, err := observabilityAssets.ReadDir("assets/dashboards")
	if err != nil {
		return nil, fmt.Errorf("embedded dashboards: %w", err)
	}
	for _, e := range entries {
		body, err := observabilityAssets.ReadFile("assets/dashboards/" + e.Name())
		if err != nil {
			return nil, err
		}
		files["dashboards/"+e.Name()] = body
	}
	return files, nil
}

func (t observabilityTopology) renderTempo(pl deploy.Placement) map[string][]byte {
	data := dataDir(t.plan, pl)
	host := t.socketHost(pl)
	retention := t.plan.Installation.Components[deploy.Tempo].Env["TEMPO_RETENTION"]
	if retention == "" {
		retention = "336h"
	}
	yaml := fmt.Sprintf(`# Rendered by operations from the applied plan. Do not edit on the host.
server:
  http_listen_address: %q
  http_listen_port: %s
  grpc_listen_address: %q
  grpc_listen_port: 9096
  log_format: json
  log_level: info

memberlist:
  bind_addr: [%q]
  bind_port: 7947
  join_members: []

query_frontend:
  search:
    max_duration: 720h
    max_result_limit: 200

compactor:
  compaction:
    block_retention: %s
    compacted_block_retention: 1h

metrics_generator:
  registry:
    external_labels:
      installation: %q
      source: tempo
  storage:
    path: %s/generator/wal
    remote_write:
      - url: "http://%s/api/v1/write"
        send_exemplars: true
  processor:
    span_metrics:
      dimensions: [service, span_name, span_kind, status_code]

storage:
  trace:
    backend: local
    wal:
      path: %s/wal
    local:
      path: %s/traces
    block:
      version: vParquet4
    pool:
      max_workers: 100
      queue_depth: 10000

overrides:
  defaults:
    metrics_generator:
      processors: [service-graphs, span-metrics]

distributor:
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: %q

ingester:
  max_block_duration: 5m
`, host, tempoHTTPPort, host, host, retention, t.plan.Installation.ID, data, t.promAddr, data, data, t.addr(pl, tempoOTLPGRPCPort))
	return map[string][]byte{"tempo.yaml": []byte(yaml)}
}

func (t observabilityTopology) renderCollector(pl deploy.Placement) map[string][]byte {
	host := t.socketHost(pl)
	yaml := fmt.Sprintf(`# Rendered by operations from the applied plan. Do not edit on the host.
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: %q
        max_recv_msg_size_mib: 16
      http:
        endpoint: %q

processors:
  memory_limiter:
    check_interval: 5s
    limit_percentage: 80
    spike_limit_percentage: 15
  resource/installation:
    attributes:
      - key: platform.installation
        value: %q
        action: upsert
  # Outcome-aware tail sampling: errors and slow traces always survive,
  # ordinary successes are sampled. Aggregates come from unsampled
  # Prometheus metrics, never from this sampled stream.
  tail_sampling:
    decision_wait: 10s
    num_traces: 50000
    expected_new_traces_per_sec: 200
    policies:
      - name: errors
        type: status_code
        status_code: {status_codes: [ERROR]}
      - name: slow
        type: latency
        latency: {threshold_ms: 1000}
      - name: baseline
        type: probabilistic
        probabilistic: {sampling_percentage: 10}
  batch:
    timeout: 5s
    send_batch_size: 1024
    send_batch_max_size: 2048

exporters:
  otlp/tempo:
    endpoint: %q
    tls: {insecure: true}
    retry_on_failure:
      enabled: true
      initial_interval: 1s
      max_interval: 30s
      max_elapsed_time: 300s
    sending_queue:
      enabled: true
      num_consumers: 4
      queue_size: 5000

extensions:
  health_check:
    endpoint: %q
    path: /healthz

service:
  extensions: [health_check]
  telemetry:
    logs: {level: info, encoding: json}
    metrics:
      level: detailed
      readers:
        - pull:
            exporter:
              prometheus:
                host: %q
                port: %s
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, resource/installation, tail_sampling, batch]
      exporters: [otlp/tempo]
`, t.addr(pl, collectorGRPCPort), t.addr(pl, collectorHTTPPort), t.plan.Installation.ID, t.tempoAddr, t.addr(pl, collectorHealthPort), host, collectorMetricPort)
	return map[string][]byte{"collector.yaml": []byte(yaml)}
}
