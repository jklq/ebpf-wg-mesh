package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/testutil"
)

// stressAgentState is the observed runtime footprint for one agent.
type stressAgentState struct {
	NetNS      map[string]bool
	Containers map[string]bool
	Desired    map[string]bool
}

// parseAgentAllocations parses the compact "key=comma-separated" output produced
// by stressAgentStateCommand.
func parseAgentAllocations(output string) (stressAgentState, error) {
	state := stressAgentState{NetNS: map[string]bool{}, Containers: map[string]bool{}, Desired: map[string]bool{}}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return stressAgentState{}, fmt.Errorf("malformed agent state line %q", line)
		}
		var target map[string]bool
		switch key {
		case "netns":
			target = state.NetNS
		case "ctr":
			target = state.Containers
		case "desired":
			target = state.Desired
		default:
			return stressAgentState{}, fmt.Errorf("unknown agent state key %q", key)
		}
		seen[key] = true
		for _, id := range strings.Split(value, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				target[id] = true
			}
		}
	}
	if !seen["netns"] || !seen["ctr"] || !seen["desired"] {
		return stressAgentState{}, fmt.Errorf("agent state output incomplete: %q", output)
	}
	return state, nil
}

// diffStressAgentState compares expected allocation IDs for one agent against
// observed containers and network namespaces. Extra containers/netns are leaks;
// a missing expected container means the plan and reality diverged.
func diffStressAgentState(agent string, expected map[string]bool, state stressAgentState) error {
	var problems []string
	for id := range state.Containers {
		if !expected[id] {
			problems = append(problems, "leaked container "+id)
		}
	}
	for id := range expected {
		if !state.Containers[id] {
			problems = append(problems, "missing container "+id)
		}
	}
	for id := range state.NetNS {
		if !expected[id] {
			problems = append(problems, "leaked netns "+id)
		}
	}
	for id := range expected {
		if !state.NetNS[id] {
			problems = append(problems, "missing netns "+id)
		}
	}
	for id := range state.Desired {
		if !expected[id] {
			problems = append(problems, "leaked desired state "+id)
		}
	}
	for id := range expected {
		if !state.Desired[id] {
			problems = append(problems, "missing desired state "+id)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("agent %s allocation state mismatch: %s", agent, strings.Join(problems, ", "))
	}
	return nil
}

func stressAgentStateCommand() string {
	return `echo "netns=$(ls /var/lib/ebpf-wg-mesh/agent/netns/*.path 2>/dev/null | sed 's#.*/##;s#\.path$##' | tr '\n' ',')"; echo "ctr=$(ctr --namespace default containers list -q 2>/dev/null | sed 's/^platform-//' | tr '\n' ',')"; echo "desired=$(ls /var/lib/ebpf-wg-mesh/agent/desired/*.json 2>/dev/null | sed 's#.*/##;s#\.json$##' | tr '\n' ',')"`
}

// checkStressLeaks requires the converged allocation set to exactly match the
// containers and netns present on each agent, so teardown leaks cannot hide.
func checkStressLeaks(ctx context.Context, o stressOptions, key string, hosts map[string]hostInfo, clients []platformv1.PlatformServiceClient, services []stressService) error {
	recoveryCtx, cancel := context.WithTimeout(ctx, o.Recovery)
	defer cancel()
	var lastErr error
	returnErr := testutil.Poll(recoveryCtx, testutil.PollConfig{Timeout: o.Recovery, Interval: 2 * time.Second}, func(ctx context.Context) (bool, error) {
		expected := make(map[string]map[string]bool)
		for i := range services {
			live, err := stressLiveAllocations(ctx, o, clients, services[i].ID)
			if err != nil {
				lastErr = err
				return false, nil
			}
			for _, allocation := range live {
				agent := allocation.GetAgentId()
				if expected[agent] == nil {
					expected[agent] = map[string]bool{}
				}
				expected[agent][allocation.GetAllocationId()] = true
			}
		}
		for name, host := range hosts {
			if name == "controlplane" {
				continue
			}
			out, err := runRemoteCommand(ctx, key, host.PublicIPv4, stressAgentStateCommand())
			if err != nil {
				lastErr = fmt.Errorf("read agent state on %s: %w", name, err)
				return false, nil
			}
			state, err := parseAgentAllocations(string(out))
			if err != nil {
				lastErr = fmt.Errorf("agent %s: %w", name, err)
				return false, nil
			}
			if err := diffStressAgentState(name, expected[name], state); err != nil {
				lastErr = err
				return false, nil
			}
		}
		return true, nil
	})
	if returnErr != nil {
		return errors.Join(returnErr, lastErr)
	}
	return nil
}
