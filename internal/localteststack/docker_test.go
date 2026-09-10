package localteststack

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestCleanupStaleLocalteststackContainersRemovesPrefixedAndLabeled(t *testing.T) {
	t.Parallel()

	runner := &cleanupDockerRunner{
		psResults: map[string]string{
			"label=platform.runtime=localteststack": "abc123\n",
			"name=localteststack-":                  "def456\nabc123\n",
		},
	}
	removed, err := CleanupStaleLocalteststackContainers(context.Background(), runner)
	if err != nil {
		t.Fatalf("CleanupStaleLocalteststackContainers: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	if len(runner.rmIDs) != 2 {
		t.Fatalf("rm ids=%v, want 2 unique ids", runner.rmIDs)
	}
	seen := map[string]bool{}
	for _, id := range runner.rmIDs {
		seen[id] = true
	}
	if !seen["abc123"] || !seen["def456"] {
		t.Fatalf("unexpected rm ids %v", runner.rmIDs)
	}
}

func TestCleanupStaleLocalteststackContainersNoopWhenEmpty(t *testing.T) {
	t.Parallel()

	runner := &cleanupDockerRunner{
		psResults: map[string]string{
			"label=platform.runtime=localteststack": "",
			"name=localteststack-":                  "",
		},
	}
	removed, err := CleanupStaleLocalteststackContainers(context.Background(), runner)
	if err != nil {
		t.Fatalf("CleanupStaleLocalteststackContainers: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed=%d, want 0", removed)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("expected only ps commands, got %+v", runner.commands)
	}
}

type cleanupDockerRunner struct {
	psResults map[string]string
	commands  [][]string
	rmIDs     []string
}

func (r *cleanupDockerRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.commands = append(r.commands, append([]string(nil), args...))
	switch {
	case len(args) >= 2 && args[0] == "ps" && args[1] == "-aq":
		var filters []string
		for i := 0; i < len(args); i++ {
			if args[i] == "--filter" && i+1 < len(args) {
				filters = append(filters, args[i+1])
				i++
			}
		}
		key := strings.Join(filters, ",")
		if out, ok := r.psResults[key]; ok {
			return []byte(out), nil
		}
		if len(filters) == 1 {
			if out, ok := r.psResults[filters[0]]; ok {
				return []byte(out), nil
			}
		}
		return nil, fmt.Errorf("unexpected ps filters %v", filters)
	case len(args) >= 2 && args[0] == "rm" && args[1] == "--force":
		r.rmIDs = append(r.rmIDs, args[2:]...)
		return []byte("removed"), nil
	default:
		return nil, fmt.Errorf("unexpected docker command: %v", args)
	}
}
