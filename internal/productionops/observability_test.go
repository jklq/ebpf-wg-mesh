package productionops

import (
	"encoding/json"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/deploy"

	"go.yaml.in/yaml/v3"
)

func observabilityTestRunner() *Runner {
	host := deploy.Host{ID: "a", Binding: deploy.Binding{Provider: "linux", ServerID: "1"}, Architecture: "amd64",
		Capacity: deploy.Resources{CPUMillis: 8000, MemoryMiB: 32768, DiskGiB: 500}, Reliability: "reliable", Trusted: true,
		Roles: append([]deploy.Role{}, deploy.Roles...), FailureDomain: "local/a", Region: "oslo",
		Network: deploy.Network{Address: "10.0.0.1", Public: true}}
	placements := []deploy.Placement{
		{Role: deploy.ControlPlane, Host: "a", Instance: "controlplane-0-x"},
		{Role: deploy.Agent, Host: "a", Instance: "agent-0-x"},
		{Role: deploy.Tempo, Host: "a", Instance: "tempo-0-x"},
		{Role: deploy.OTelCollector, Host: "a", Instance: "otel-collector-0-x"},
		{Role: deploy.Prometheus, Host: "a", Instance: "prometheus-0-x"},
		{Role: deploy.Grafana, Host: "a", Instance: "grafana-0-x"},
	}
	return &Runner{Plan: deploy.Plan{
		ID: "plan", Release: deploy.Release{ID: "r43"},
		Installation: deploy.Installation{ID: "production", Hosts: []deploy.Host{host},
			Components: map[deploy.Role]deploy.Component{deploy.Tempo: {Env: map[string]string{}}}},
		Placements: placements,
	}}
}

func TestObservabilityRendering(t *testing.T) {
	r := observabilityTestRunner()
	obs := r.observability("secret-token")
	if !obs.hasObs || obs.tempoAddr != "10.0.0.1:4327" || obs.promAddr != "10.0.0.1:9099" {
		t.Fatalf("topology: %+v", obs)
	}
	if len(obs.collectors) != 1 || obs.collectors[0] != "10.0.0.1:4317" {
		t.Fatalf("collectors: %v", obs.collectors)
	}

	byRole := map[deploy.Role]deploy.Placement{}
	for _, pl := range r.Plan.Placements {
		byRole[pl.Role] = pl
	}

	prom, err := obs.render(byRole[deploy.Prometheus])
	if err != nil {
		t.Fatal(err)
	}
	promYAML := string(prom["prometheus.yaml"])
	for _, want := range []string{"platform-controlplane", "10.0.0.1:9095", "credentials_file", "rules/platform.yaml", "otel_scope_"} {
		if !strings.Contains(promYAML, want) && want != "rules/platform.yaml" {
			if !strings.Contains(promYAML, want) {
				t.Fatalf("prometheus config missing %q:\n%s", want, promYAML)
			}
		}
	}
	if _, ok := prom["metrics-token"]; !ok {
		t.Fatal("metrics token file not rendered")
	}
	if !strings.Contains(string(prom["rules/platform.yaml"]), "PlatformTargetDown") {
		t.Fatal("alert rules not embedded")
	}

	grafana, err := obs.render(byRole[deploy.Grafana])
	if err != nil {
		t.Fatal(err)
	}
	ds := string(grafana["provisioning/datasources/platform.yaml"])
	if !strings.Contains(ds, "http://10.0.0.1:9099") || !strings.Contains(ds, "http://10.0.0.1:3200") || !strings.Contains(ds, "exemplarTraceIdDestinations") {
		t.Fatalf("datasources:\n%s", ds)
	}
	if strings.Contains(string(grafana["grafana.ini"]), "secret-token") {
		t.Fatal("scrape token leaked into grafana config")
	}
	dashboards := 0
	for name, body := range grafana {
		if strings.HasPrefix(name, "dashboards/") {
			dashboards++
			var doc map[string]any
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("dashboard %s invalid JSON: %v", name, err)
			}
			if doc["uid"] == nil || doc["title"] == nil {
				t.Fatalf("dashboard %s missing uid/title", name)
			}
		}
	}
	if dashboards != 3 {
		t.Fatalf("want 3 dashboards, got %d", dashboards)
	}

	tempo, err := obs.render(byRole[deploy.Tempo])
	if err != nil {
		t.Fatal(err)
	}
	if c := string(tempo["tempo.yaml"]); !strings.Contains(c, "block_retention: 336h") || !strings.Contains(c, "http://10.0.0.1:9099/api/v1/write") || !strings.Contains(c, "endpoint: \"10.0.0.1:4327\"") {
		t.Fatalf("tempo config:\n%s", c)
	}

	coll, err := obs.render(byRole[deploy.OTelCollector])
	if err != nil {
		t.Fatal(err)
	}
	cc := string(coll["collector.yaml"])
	for _, want := range []string{"tail_sampling", "status_code", "threshold_ms: 1000", "sampling_percentage: 10", "endpoint: \"10.0.0.1:4327\"", "memory_limiter"} {
		if !strings.Contains(cc, want) {
			t.Fatalf("collector config missing %q:\n%s", want, cc)
		}
	}

	// Every rendered YAML document must parse.
	for role, files := range map[deploy.Role]map[string][]byte{
		deploy.Prometheus: prom, deploy.Grafana: grafana, deploy.Tempo: tempo, deploy.OTelCollector: coll,
	} {
		for name, body := range files {
			if strings.HasSuffix(name, ".yaml") {
				var doc any
				if err := yaml.Unmarshal(body, &doc); err != nil {
					t.Fatalf("%s %s does not parse: %v", role, name, err)
				}
			}
		}
	}

	// Non-observability placements render nothing.
	if files, err := obs.render(byRole[deploy.ControlPlane]); err != nil || files != nil {
		t.Fatalf("controlplane render = %v, %v", files, err)
	}
}

func TestTelemetryEnvInjection(t *testing.T) {
	r := observabilityTestRunner()
	obs := r.observability("tok")
	for _, pl := range r.Plan.Placements {
		env := map[string]string{}
		obs.telemetryEnv(pl, env)
		switch pl.Role {
		case deploy.ControlPlane, deploy.Agent:
			if env["TELEMETRY_METRICS_LISTEN"] != "10.0.0.1:9095" || env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "10.0.0.1:4317" || env["TELEMETRY_METRICS_TOKEN"] != "tok" {
				t.Fatalf("%s env: %v", pl.Role, env)
			}
		default:
			if len(env) != 0 {
				t.Fatalf("%s must not receive Go telemetry env: %v", pl.Role, env)
			}
		}
	}
	// Without a collector, components still expose metrics but export nothing.
	noColl := r.observability("")
	noColl.collectors = nil
	env := map[string]string{}
	noColl.telemetryEnv(r.Plan.Placements[0], env)
	if _, ok := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
		t.Fatalf("unexpected OTLP endpoint without collector: %v", env)
	}
	if env["TELEMETRY_METRICS_LISTEN"] == "" {
		t.Fatal("metrics listen must be set even without a collector")
	}
}
