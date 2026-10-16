# Observability

The platform emits three correlated signals from every Go component
(`controlplane`, `agent`, `builder`):

| Signal | Mechanism | Where it lands |
| --- | --- | --- |
| Metrics | OpenTelemetry instruments, scraped as Prometheus exposition with exemplars | Prometheus (bundled role) or any Prometheus/OTLP backend |
| Traces | OTLP/gRPC, W3C `traceparent` across gRPC, Connect and HTTP hops | OTel Collector (tail sampling) → Tempo |
| Logs | Structured `slog` JSON on stderr (systemd journal), one **wide event** per completed operation per service hop, carrying `trace_id`/`span_id` | Journal today; forward with your standard shipper |

Workload (tenant) logs are a separate system (agent → control plane →
ClickHouse/file) and are unchanged. This document covers platform telemetry
only.

## Design rules

- **One wide event per operation per hop.** Middleware, auth and handlers
  enrich a single operation record (`telemetry.Set(ctx, …)`); it finalizes
  once with outcome, stable `error_kind` (gRPC code name), duration, caller
  and entity IDs. To investigate: find the failed wide event by
  `trace_id`, open the same trace in Tempo, compare by `caller.class`,
  release, or route.
- **Cardinality contract.** Metric labels are bounded only (outcome, kind,
  method, executor, loop). IDs live on spans and wide events, never labels.
  DB spans carry the operation class, not statement text
  (`disable_query`), so no literals or secrets reach telemetry.
- **Sampling is outcome-aware, at the Collector.** SDK head sampling
  defaults to 100%; the bundled Collector tail-samples: errors and traces
  slower than 1s always survive, ordinary successes keep 10%. Rates and
  percentiles come from unsampled metrics, never from sampled traces.
- **Telemetry never breaks work.** Setup failure disables export and logs
  a warning; every recording helper is a no-op when telemetry is off; the
  OTLP exporter drops rather than blocks.

## Configuration (environment)

Telemetry follows the OpenTelemetry env convention; the deployment sets
these for you when observability roles are enabled.

| Variable | Default | Meaning |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | – | Collector `host:4317` (or any OTLP/gRPC backend). Empty: traces stay local (exemplars and log correlation still work) |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Plaintext to the collector (bundled stack sets `true`; traffic stays on the private host network) |
| `OTEL_SERVICE_NAME` | component name | Normally unset; binaries name themselves |
| `TELEMETRY_METRICS_LISTEN` | – | Dedicated Prometheus listener, e.g. `10.0.0.1:9095`. When empty, `/metrics` shares the component health listener |
| `TELEMETRY_METRICS_TOKEN` | – | Bearer token required on `/metrics`. Deployments inject one shared token |
| `TELEMETRY_TRACES_SAMPLE_RATIO` | `1` | Head-sampling ratio for root spans; leave at `1` and let the Collector tail-sample |
| `TELEMETRY_LOG_FORMAT` | `json` | `json`, `text`, or `none` (leave the process default alone) |
| `TELEMETRY_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `TELEMETRY_DISABLED` | `false` | Kills export; metrics remain scrapeable on request |

Standard `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_*`/`OTEL_METRICS_*`
variables also apply.

## Metric catalogue (high level)

Transport (OTel semantic conventions, `_seconds` histograms with exemplars):
`rpc_server_call_duration_seconds`, `rpc_client_call_duration_seconds`,
`http_server_request_duration_seconds`, `http_client_request_duration_seconds`,
`db_client_operation_duration_seconds`, plus Go runtime and process
collectors (`go_*`, `process_*`) and DB pool gauges
(`db_sql_connection_open/max_open/wait_total`).

Domain (`platform_*`): `platform_operations_total{kind,outcome}` +
`platform_operation_duration_seconds`, `platform_builds_total{outcome,executor}` +
`platform_build_duration_seconds`, `platform_agent_sessions_total{outcome}`,
`platform_reconcile_runs_total{loop,outcome}`, `platform_log_lines_total`,
`platform_log_drops_total`.

## Bundled deployment (optional roles)

Four optional `platformctl` roles place like any other component. Omit
them (or `replicas: 0`) and the platform runs identically; their loss
never gates platform availability, and no public endpoint is required —
Grafana stays on the private host address.

| Role | Listens (host private address) | State |
| --- | --- | --- |
| `otel-collector` | OTLP `4317`/`4318`, health `13133`, own metrics `8888` | stateless |
| `tempo` | API `3200`, OTLP ingest `4327` | traces + WAL under its state dir, 14d retention (`TEMPO_RETENTION` component env) |
| `prometheus` | `9099` | TSDB under its state dir, 30d retention (release Program args) |
| `grafana` | `3001` | sqlite under its state dir; datasources/dashboards provisioned, UI edits off |

Enable by adding host roles + components (see
`infra/production/examples/single-host.yaml`), plus:

1. A `grafana-admin-password` secret, wired as
   `secretEnv: {GF_SECURITY_ADMIN_PASSWORD: grafana-admin-password}`.
2. Probes for the four roles in the operations config (see
   `infra/production/operations/config.example.json`), and optionally
   `metricsTokenFile` for the shared scrape token.
3. Native inputs at packaging time: `prometheus`, `grafana` (tree with
   entry `[bin/grafana, server]`), `tempo`, `otel-collector`
   (otelcol-contrib). Omitting them drops only those programs from the
   release. Pins live in the release `dependencies` block.

Configuration files (Prometheus scrape config + rules, Grafana
provisioning + dashboards, Tempo, Collector) are rendered from the
applied plan by the operations tool on every credentials run — do not
edit them on hosts. Dashboards and rules are versioned in
`internal/productionops/assets/`.

Access Grafana over the private network, for example
`ssh -L 3001:<host-address>:3001 <host>` then `http://127.0.0.1:3001`,
or front it with your own authenticated proxy. Change the admin password
secret and create named users before sharing access; anonymous access
and sign-up are disabled.

### Sizing and retention guidance

Single-host defaults (0.5–1 CPU, 0.5–1 GiB per role) suit small fleets.
Budget roughly: Prometheus ~1–2 bytes/sample compressed — the platform's
series count is dominated by per-method histograms, so watch
`prometheus_tsdb_head_series` (alert at 500k). Tempo local storage holds
only sampled traces; at 200 traces/s ingest with the default policies,
14 days is on the order of tens of GiB — set `TEMPO_RETENTION` and the
component `diskGiB` together. Run at most one Prometheus unless you
deliberately want duplicate scrapes for HA; Grafana reads the first.

## Alerts

`assets/rules/platform.yaml` ships recording rules plus alerts for target
down, RPC error ratio/latency, DB pool saturation and slow operations,
build failure rate, agent session flapping, log drops, trace export
failure and cardinality jumps. Every alert annotation names the next
operator action. Route them with Alertmanager or Grafana Alerting to the
same destination as platform alarms — Prometheus rules evaluate in the
bundled Prometheus with no extra wiring.

## Using an external backend instead

Skip the bundled roles and set `OTEL_EXPORTER_OTLP_ENDPOINT` (and TLS, so
leave `OTEL_EXPORTER_OTLP_INSECURE` unset) on the Go components via their
component `env`. Point any Prometheus at each component's `:9095/metrics`
(set `TELEMETRY_METRICS_LISTEN` to the private address) with the scrape
token, and import the dashboards from `internal/productionops/assets/`.

## Known gaps

- No `node_exporter` role: host-level CPU/disk comes from Go process
  collectors per component, not whole-host saturation. Add a node
  exporter out of band if you need it.
- Envoy and PostgreSQL are not scraped (admin/metrics stay loopback or
  need an exporter sidecar); control-plane DB client metrics and Envoy's
  effect on RPC traces cover the common investigations.
- Console (TypeScript) requests join traces only at the control-plane
  hop; browser RUM is out of scope.
- Platform logs are not exported over OTLP; ship journald with your
  standard forwarder and join on `trace_id`.
