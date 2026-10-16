# Observability deployment assets

Dashboards and Prometheus rules are versioned with the operations tool in
`internal/productionops/assets/` (embedded and rendered into each instance
at deploy time). Operator guidance, configuration, sizing and access live in
[docs/observability.md](../../../docs/observability.md). The example
installation with the stack enabled is `../examples/single-host.yaml`.
