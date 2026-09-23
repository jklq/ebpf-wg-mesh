// Package xds is the control-plane xDS authority for the Envoy ingress data
// plane. Snapshots are a deterministic function of control-plane state:
// replicas racing to compute from the same state produce identical bytes and
// converge on one version. A NACK never withdraws the last published snapshot.
package xds

type Backend struct {
	Domain       string
	Upstream     string
	AllocationID string
}

type StaticRoute struct {
	Hosts    []string
	Upstream string
}
