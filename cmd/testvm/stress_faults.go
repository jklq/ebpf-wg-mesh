package main

import (
	"fmt"
	"time"
)

// stressFault kinds span independent failure domains so a campaign can compound
// two without trivially cascading. Scope picks companions from another domain.
const (
	faultScopeControlPlane = "controlplane"
	faultScopeAgent        = "agent"
	faultScopeDatabase     = "database"
)

type stressFaultKind struct {
	Name string
	// Scope groups faults sharing a failure domain; companions come from elsewhere.
	Scope string
	// DataPlaneSafe is false when the fault may break workload traffic.
	DataPlaneSafe bool
	// HostScope is the host the fault runs against.
	HostScope string
}

var stressFaultKinds = []stressFaultKind{
	{Name: "controlplane-kill", Scope: faultScopeControlPlane, DataPlaneSafe: true, HostScope: "controlplane"},
	{Name: "controlplane-pause", Scope: faultScopeControlPlane, DataPlaneSafe: true, HostScope: "controlplane"},
	{Name: "cpu-throttle", Scope: faultScopeControlPlane, DataPlaneSafe: true, HostScope: "controlplane"},
	{Name: "memory-pressure", Scope: faultScopeControlPlane, DataPlaneSafe: true, HostScope: "controlplane"},
	{Name: "database-pause", Scope: faultScopeDatabase, DataPlaneSafe: true, HostScope: "controlplane"},
	{Name: "database-restart", Scope: faultScopeDatabase, DataPlaneSafe: true, HostScope: "controlplane"},
	{Name: "disk-io-throttle", Scope: faultScopeDatabase, DataPlaneSafe: true, HostScope: "controlplane"},
	{Name: "agent-kill", Scope: faultScopeAgent, DataPlaneSafe: false, HostScope: "agent"},
	{Name: "agent-partition", Scope: faultScopeAgent, DataPlaneSafe: true, HostScope: "agent"},
	{Name: "agent-wg-partition", Scope: faultScopeAgent, DataPlaneSafe: false, HostScope: "agent"},
	{Name: "clock-skew", Scope: faultScopeAgent, DataPlaneSafe: true, HostScope: "agent"},
}

// stressCoreFaults are scheduled first so a default campaign covers them all;
// extended faults only appear in longer campaigns.
var stressCoreFaults = map[string]bool{
	"controlplane-kill":  true,
	"controlplane-pause": true,
	"agent-kill":         true,
	"agent-partition":    true,
	"database-pause":     true,
	"cpu-throttle":       true,
	"memory-pressure":    true,
}

func stressFaultKindByName(name string) (stressFaultKind, bool) {
	for _, kind := range stressFaultKinds {
		if kind.Name == name {
			return kind, true
		}
	}
	return stressFaultKind{}, false
}

// stressFaultSteps resolves a stage's faults into concrete remote commands. Heal
// is a function so clock repair embeds the wall time at heal time.
type stressFaultStep struct {
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Inject string `json:"inject"`
	Heal   func() string
	Ready  string `json:"ready,omitempty"`
}

func constHeal(command string) func() string {
	return func() string { return command }
}

func stressFaultIsDataPlaneSafe(name string) bool {
	kind, ok := stressFaultKindByName(name)
	return ok && kind.DataPlaneSafe
}

func stressFaultSteps(hosts map[string]hostInfo, ownerService string, faults []stressFault) ([]stressFaultStep, error) {
	var steps []stressFaultStep
	for _, fault := range faults {
		switch fault.Kind {
		case "controlplane-kill":
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: "controlplane",
				Inject: "systemctl kill --kill-whom=main --signal=SIGKILL " + ownerService,
				Heal:   constHeal("systemctl start " + ownerService),
				Ready:  "systemctl is-active --quiet " + ownerService})
		case "controlplane-pause":
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: "controlplane",
				Inject: "systemctl kill --kill-whom=main --signal=SIGSTOP " + ownerService,
				Heal:   constHeal("systemctl kill --kill-whom=main --signal=SIGCONT " + ownerService)})
		case "cpu-throttle":
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: "controlplane",
				Inject: "systemctl set-property --runtime " + ownerService + " CPUQuota=10%",
				Heal:   constHeal("systemctl set-property --runtime " + ownerService + " CPUQuota=")})
		case "memory-pressure":
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: "controlplane",
				Inject: "systemctl set-property --runtime " + ownerService + " MemoryHigh=48M MemoryMax=96M",
				Heal:   constHeal("systemctl set-property --runtime " + ownerService + " MemoryHigh=infinity MemoryMax=infinity")})
		case "database-pause":
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: "controlplane",
				Inject: "systemctl kill --kill-whom=main --signal=SIGSTOP ebpf-wg-mesh-cockroach",
				Heal:   constHeal("systemctl kill --kill-whom=main --signal=SIGCONT ebpf-wg-mesh-cockroach")})
		case "database-restart":
			// A real crash/restart, not a pause; the heal is idempotent.
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: "controlplane",
				Inject: "systemctl restart ebpf-wg-mesh-cockroach",
				Heal:   constHeal("systemctl start ebpf-wg-mesh-cockroach"),
				Ready:  "systemctl is-active --quiet ebpf-wg-mesh-cockroach && cockroach sql --insecure --host=127.0.0.1:26257 --execute 'SELECT 1' >/dev/null"})
		case "disk-io-throttle":
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: "controlplane",
				Inject: "systemctl set-property --runtime ebpf-wg-mesh-cockroach IOWriteBandwidthMax='/ 1048576' IOWeight=1",
				Heal:   constHeal("systemctl set-property --runtime ebpf-wg-mesh-cockroach IOWriteBandwidthMax= IOWeight=100")})
		case "agent-kill":
			host, err := stressFaultHost(hosts, fault.Target)
			if err != nil {
				return nil, err
			}
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: host,
				Inject: "systemctl kill --kill-whom=main --signal=SIGKILL ebpf-wg-mesh-agent",
				Heal:   constHeal("systemctl start ebpf-wg-mesh-agent"),
				Ready:  "systemctl is-active --quiet ebpf-wg-mesh-agent"})
		case "agent-partition":
			host, err := stressFaultHost(hosts, fault.Target)
			if err != nil {
				return nil, err
			}
			controlPlaneIP := hosts["controlplane"].PublicIPv4
			outgoing := "iptables -I OUTPUT -p tcp -d " + controlPlaneIP + " -m multiport --dports 9443,9444 -m comment --comment mesh-stress -j DROP"
			incoming := "iptables -I INPUT -p tcp -s " + controlPlaneIP + " -m multiport --sports 9443,9444 -m comment --comment mesh-stress -j DROP"
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: host,
				Inject: outgoing + " && " + incoming,
				Heal:   constHeal("iptables -D " + ruleBody(outgoing) + "; e1=$?; iptables -D " + ruleBody(incoming) + "; e2=$?; test $e1 -eq 0 && test $e2 -eq 0")})
		case "agent-wg-partition":
			// Cut the agent's data plane only, so the mesh must route around it.
			host, err := stressFaultHost(hosts, fault.Target)
			if err != nil {
				return nil, err
			}
			outgoing := "iptables -I OUTPUT -p udp --dport 51820 -m comment --comment mesh-wg-stress -j DROP"
			incoming := "iptables -I INPUT -p udp --sport 51820 -m comment --comment mesh-wg-stress -j DROP"
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: host,
				Inject: outgoing + " && " + incoming,
				Heal:   constHeal("iptables -D " + ruleBody(outgoing) + "; e1=$?; iptables -D " + ruleBody(incoming) + "; e2=$?; test $e1 -eq 0 && test $e2 -eq 0")})
		case "clock-skew":
			host, err := stressFaultHost(hosts, fault.Target)
			if err != nil {
				return nil, err
			}
			// Bounded skew: exercises time assumptions inside TLS/JWT windows.
			steps = append(steps, stressFaultStep{Kind: fault.Kind, Host: host,
				Inject: "timedatectl set-ntp false && date -u -s \"$(date -u -d '+5 seconds')\"",
				Heal: func() string {
					return fmt.Sprintf("date -u -s @%d; timedatectl set-ntp true", time.Now().Unix())
				}})
		default:
			return nil, fmt.Errorf("unknown stress fault %q", fault.Kind)
		}
	}
	return steps, nil
}

// ruleBody strips the leading "iptables -I " so a heal can reuse the rule with "-D".
func ruleBody(insertCommand string) string {
	const prefix = "iptables -I "
	if len(insertCommand) > len(prefix) && insertCommand[:len(prefix)] == prefix {
		return insertCommand[len(prefix):]
	}
	return insertCommand
}

func stressFaultHost(hosts map[string]hostInfo, target string) (string, error) {
	if _, ok := hosts[target]; ok {
		return target, nil
	}
	// Fallback to the lexicographically smallest agent so runs stay replayable.
	best := ""
	for name := range hosts {
		if name == "controlplane" {
			continue
		}
		if best == "" || name < best {
			best = name
		}
	}
	if best == "" {
		return "", fmt.Errorf("no agent host available for fault target %q", target)
	}
	return best, nil
}
