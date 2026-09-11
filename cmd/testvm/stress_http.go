package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/testutil"
)

// stressHTTPReport is the remote Python generator's JSON schema. It carries the
// raw histogram so fanout flows can be merged before percentiles are computed.
type stressHTTPReport struct {
	Requests int64            `json:"requests"`
	Errors   int64            `json:"errors"`
	Codes    map[string]int64 `json:"codes"`
	Seconds  float64          `json:"seconds"`
	RPS      float64          `json:"requests_per_second"`
	P50MS    int              `json:"p50_ms"`
	P95MS    int              `json:"p95_ms"`
	P99MS    int              `json:"p99_ms"`
	Buckets  []int64          `json:"buckets"`
}

type httpFlow struct {
	Source           stressService
	Target           stressService
	SourceAllocation *platformv1.AllocationStatus
	TargetAllocation *platformv1.AllocationStatus
}

func reserveHTTPFlows(flows []httpFlow, reserved map[int]bool) {
	for _, flow := range flows {
		reserved[flow.Source.Index] = true
		reserved[flow.Target.Index] = true
	}
}

// planStressHTTP resolves a healthy allocation for each service and picks a
// bounded fanout of same-project source→target flows, preferring a target on a
// different agent so the WireGuard/eBPF path is exercised.
func planStressHTTP(ctx context.Context, o stressOptions, client platformv1.PlatformServiceClient, services []stressService, tolerant bool) ([]httpFlow, error) {
	allocations := make(map[string]*platformv1.AllocationStatus, len(services))
	for _, service := range services {
		var allocation *platformv1.AllocationStatus
		err := testutil.Poll(ctx, testutil.PollConfig{Timeout: min(o.RPCTimeout*3, 15*time.Second), Interval: time.Second}, func(ctx context.Context) (bool, error) {
			callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
			defer cancel()
			state, err := client.GetServiceStatus(callCtx, &platformv1.GetServiceStatusRequest{ServiceId: service.ID})
			if err != nil {
				if stressAmbiguous(err) {
					return false, nil
				}
				return false, err
			}
			allocation = matchingHealthyAllocation(state, "", service.Revision, 1)
			return allocation != nil, nil
		})
		if err != nil {
			if tolerant {
				continue
			}
			return nil, fmt.Errorf("resolve HTTP allocation for %s: %w", service.ID, err)
		}
		allocations[service.ID] = allocation
	}
	return chooseHTTPFlows(services, allocations, o.HTTPTargets), nil
}

// chooseHTTPFlows spreads a bounded number of flows across services and keeps
// each flow within one project so the mesh isolation policy is not the thing
// under test here.
func chooseHTTPFlows(services []stressService, allocations map[string]*platformv1.AllocationStatus, targets int) []httpFlow {
	if targets < 1 {
		targets = 1
	}
	byEnvironment := make(map[string][]stressService)
	for _, service := range services {
		if allocations[service.ID] == nil {
			continue
		}
		byEnvironment[service.Environment] = append(byEnvironment[service.Environment], service)
	}
	candidates := make([]stressService, 0, len(services))
	for _, service := range services {
		if allocations[service.ID] != nil {
			candidates = append(candidates, service)
		}
	}
	step := max(1, (len(candidates)+targets-1)/targets)
	var flows []httpFlow
	for i := 0; i < len(candidates) && len(flows) < targets; i += step {
		target := candidates[i]
		targetAllocation := allocations[target.ID]
		if targetAllocation == nil {
			continue
		}
		var source stressService
		var fallback stressService
		for _, peer := range byEnvironment[target.Environment] {
			if peer.ID == target.ID {
				continue
			}
			if fallback.ID == "" {
				fallback = peer
			}
			if allocations[peer.ID].GetAgentId() != targetAllocation.GetAgentId() {
				source = peer
				break
			}
		}
		if source.ID == "" {
			source = fallback
		}
		if source.ID == "" {
			// A one-service environment can still exercise the HTTP path from
			// its own workload namespace. Prefer a peer whenever one exists.
			source = target
		}
		flows = append(flows, httpFlow{
			Source:           source,
			Target:           target,
			SourceAllocation: allocations[source.ID],
			TargetAllocation: targetAllocation,
		})
	}
	return flows
}

// runStressHTTPFlows executes each planned flow in parallel inside its source
// allocation's network namespace. In tolerant mode a flow that cannot start is
// recorded and skipped; wrong content is always a hard failure because the
// planned source/target services are excluded from concurrent mutation.
func runStressHTTPFlows(ctx context.Context, o stressOptions, stage stressStage, flows []httpFlow, key string, hosts map[string]hostInfo, trace *stressTrace, tolerant bool, repoRoot string) (*stressStats, error) {
	report := &stressStats{Codes: make(map[string]int64)}
	if len(flows) == 0 {
		if tolerant {
			return report, nil
		}
		return nil, fmt.Errorf("no healthy service allocation for HTTP load")
	}
	script, err := os.ReadFile(filepath.Join(repoRoot, "infra/test-vm/remote/stress-http.py"))
	if err != nil {
		return nil, err
	}
	perFlowConcurrency := max(1, stage.Concurrency/len(flows))
	descriptors := make([]map[string]any, 0, len(flows))
	for _, flow := range flows {
		descriptors = append(descriptors, map[string]any{"source_agent": flow.SourceAllocation.GetAgentId(), "target_agent": flow.TargetAllocation.GetAgentId(), "target_service": flow.Target.ID})
	}
	trace.record(stage.Number, "http-flows", map[string]any{"flows": descriptors, "concurrency_per_flow": perFlowConcurrency})

	results := make([]stressHTTPReport, len(flows))
	errs := make([]error, len(flows))
	started := time.Now()
	var wg sync.WaitGroup
	for i := range flows {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = runHTTPFlow(ctx, o, stage, flows[i], perFlowConcurrency, key, hosts, script)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			if !tolerant {
				return nil, fmt.Errorf("HTTP flow %d: %w", i, err)
			}
			trace.record(stage.Number, "http-flow-failed", map[string]any{"flow": i, "error": err.Error()})
			report.Requests++
			report.Errors++
			report.Codes["flow-start-failed"]++
			continue
		}
		report.fold(results[i])
	}
	report.finish(time.Since(started))
	trace.record(stage.Number, "http-load-result", report)
	if report.Codes["wrong-content"] > 0 {
		return report, fmt.Errorf("HTTP load returned wrong content %d times", report.Codes["wrong-content"])
	}
	return report, nil
}

func runHTTPFlow(ctx context.Context, o stressOptions, stage stressStage, flow httpFlow, concurrency int, key string, hosts map[string]hostInfo, script []byte) (stressHTTPReport, error) {
	host, ok := hosts[flow.SourceAllocation.GetAgentId()]
	if !ok {
		return stressHTTPReport{}, fmt.Errorf("unknown HTTP source agent %s", flow.SourceAllocation.GetAgentId())
	}
	netnsPath := "/var/lib/ebpf-wg-mesh/agent/netns/" + flow.SourceAllocation.GetAllocationId() + ".path"
	command := fmt.Sprintf(`ns=$(cat %s) && nsenter --net="$ns" python3 - %.3f %d %.3f %s %s`, shellQuote(netnsPath), o.StageDuration.Seconds(), concurrency, o.RPCTimeout.Seconds(), shellQuote("http://"+allocationEndpoint(flow.TargetAllocation)+"/"), shellQuote(flow.Target.Marker))
	runCtx, cancel := context.WithTimeout(ctx, o.StageDuration+o.RPCTimeout+15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "ssh", sshArgs(key, host.PublicIPv4, command)...)
	cmd.Stdin = bytes.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return stressHTTPReport{}, fmt.Errorf("remote HTTP load generator: %w: %s", err, stderr.String())
	}
	var report stressHTTPReport
	if err := json.Unmarshal(out, &report); err != nil {
		return stressHTTPReport{}, fmt.Errorf("HTTP load report: %w", err)
	}
	return report, nil
}
