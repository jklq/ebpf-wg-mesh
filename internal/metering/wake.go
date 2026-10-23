package metering

// WakeSource reports cumulative packets addressed to local identities with
// no live workload (sleeping serverless allocations), keyed by normalized
// destination IP (IPv4 as v4-mapped IPv6). Consumers diff successive reads;
// the packets themselves were dropped by the firewall.
type WakeSource interface {
	ReadWake() (map[[16]byte]uint64, error)
}

// WakeCountersPinPath is where the firewall pins the wake counters map.
func WakeCountersPinPath(iface string) string {
	if iface == "" {
		iface = "wg0"
	}
	return "/sys/fs/bpf/ebpf-wg-mesh/" + iface + "/maps/wake_counters"
}
