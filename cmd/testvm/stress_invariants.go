package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func replicaRPCRedirected(err error) bool {
	if status.Code(err) != codes.FailedPrecondition {
		return false
	}
	_, ok := delivery.ParseLiveOwnerRedirect(status.Convert(err).Message())
	return ok
}

func checkStressService(expected stressService, actual *platformv1.Service) error {
	if actual.GetId() != expected.ID || actual.GetEnvironmentId() != expected.Environment {
		return fmt.Errorf("service identity changed: %s", expected.ID)
	}
	marker := actual.GetSpec().GetRuntime().GetEnv()["MARKER"]
	valid := marker == expected.Marker
	for _, alternative := range expected.Alternatives {
		if marker == alternative {
			valid = true
			break
		}
	}
	if !valid || actual.GetSpecRevision() < expected.Revision {
		return fmt.Errorf("acknowledged state lost for %s: marker=%q revision=%d, expected %q (or an attempted value) revision >=%d", expected.ID, marker, actual.GetSpecRevision(), expected.Marker, expected.Revision)
	}
	return nil
}

// retainStressError keeps the most recent meaningful failure across poll
// retries. The final attempt of a timing-out poll usually fails with the poll
// context's DeadlineExceeded, which must not mask the persistent cause, so a
// non-transient error replaces any prior error (including another
// non-transient one) while a transient error only fills an empty slot.
func retainStressError(current *error, err error) {
	if err == nil {
		return
	}
	if *current == nil || !transientContextError(err) {
		*current = err
		infof("stress invariant error: %v", err)
	}
}

func transientContextError(err error) bool {
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Canceled:
		return true
	default:
		return false
	}
}

func environmentSet(services []stressService) []string {
	seen := map[string]bool{}
	var environments []string
	for _, service := range services {
		if !seen[service.Environment] {
			seen[service.Environment] = true
			environments = append(environments, service.Environment)
		}
	}
	return environments
}

// stressHealthyAllocation requires exactly one healthy serving allocation for a
// service at convergence and returns it. Two serving allocations are a
// scheduling split-brain bug; a non-serving allocation may still be draining.
func stressHealthyAllocation(ctx context.Context, o stressOptions, clients []platformv1.PlatformServiceClient, serviceID string, revision int64) (*platformv1.AllocationStatus, error) {
	var lastErr error
	for _, client := range clients {
		callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
		state, err := client.GetServiceStatus(callCtx, &platformv1.GetServiceStatusRequest{ServiceId: serviceID})
		cancel()
		if err != nil {
			if status.Code(err) != codes.FailedPrecondition {
				retainStressError(&lastErr, fmt.Errorf("GetServiceStatus %s: %w", serviceID, err))
			}
			continue
		}
		serving := 0
		for _, allocation := range state.GetAllocations() {
			if allocation.GetHealthy() && allocation.GetRolloutState() == "serving" {
				serving++
			}
		}
		if serving != 1 {
			return nil, fmt.Errorf("service %s has %d healthy serving allocations at convergence, want exactly 1: %s", serviceID, serving, formatServiceAllocations(state))
		}
		allocation := matchingHealthyAllocation(state, "", revision, 1)
		if allocation == nil {
			lastErr = fmt.Errorf("service %s has no healthy serving allocation at revision >=%d: %s", serviceID, revision, formatServiceAllocations(state))
			continue
		}
		return allocation, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("service %s status unavailable from every replica", serviceID)
	}
	return nil, lastErr
}

// stressLiveAllocations returns every allocation the control plane still tracks
// for a service so draining and withdrawing resources are not mistaken for
// leaks before teardown completes.
func stressLiveAllocations(ctx context.Context, o stressOptions, clients []platformv1.PlatformServiceClient, serviceID string) ([]*platformv1.AllocationStatus, error) {
	var lastErr error
	for _, client := range clients {
		callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
		state, err := client.GetServiceStatus(callCtx, &platformv1.GetServiceStatusRequest{ServiceId: serviceID})
		cancel()
		if err != nil {
			if status.Code(err) != codes.FailedPrecondition {
				retainStressError(&lastErr, fmt.Errorf("GetServiceStatus %s: %w", serviceID, err))
			}
			continue
		}
		live := make([]*platformv1.AllocationStatus, 0, len(state.GetAllocations()))
		for _, allocation := range state.GetAllocations() {
			if allocation.GetRolloutState() == "lost" {
				continue
			}
			live = append(live, allocation)
		}
		return live, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("service %s status unavailable from every replica", serviceID)
	}
	return nil, lastErr
}

func verifyStress(ctx context.Context, o stressOptions, clients []platformv1.PlatformServiceClient, services []stressService, key string, hosts map[string]hostInfo) error {
	services = append([]stressService(nil), services...)
	recoveryCtx, cancel := context.WithTimeout(ctx, o.Recovery)
	defer cancel()
	var lastErr error
	returnErr := testutil.Poll(recoveryCtx, testutil.PollConfig{Timeout: o.Recovery, Interval: 2 * time.Second}, func(ctx context.Context) (bool, error) {
		byEnvironment := make(map[string]map[string]bool)
		for _, service := range services {
			if byEnvironment[service.Environment] == nil {
				byEnvironment[service.Environment] = make(map[string]bool)
			}
			byEnvironment[service.Environment][service.ID] = true
		}
		for environment, expected := range byEnvironment {
			for clientIndex, client := range clients {
				callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
				actual, err := client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: environment})
				cancel()
				if err != nil {
					if replicaRPCRedirected(err) {
						continue
					}
					retainStressError(&lastErr, fmt.Errorf("ListServices env %s client %d: %w", environment, clientIndex, err))
					return false, nil
				}
				if err := checkStressList(environment, expected, actual); err != nil {
					retainStressError(&lastErr, err)
					return false, nil
				}
			}
		}
		allocations := make([]*platformv1.AllocationStatus, len(services))
		for i := range services {
			expected := &services[i]
			var resolvedMarker string
			var resolvedRevision int64
			served := 0
			for clientIndex, client := range clients {
				callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
				actual, err := client.GetService(callCtx, &platformv1.GetServiceRequest{ServiceId: expected.ID})
				cancel()
				if err != nil {
					if replicaRPCRedirected(err) {
						continue
					}
					retainStressError(&lastErr, fmt.Errorf("GetService %s client %d: %w", expected.ID, clientIndex, err))
					return false, nil
				}
				if err := checkStressService(*expected, actual); err != nil {
					retainStressError(&lastErr, err)
					return false, nil
				}
				marker := actual.GetSpec().GetRuntime().GetEnv()["MARKER"]
				served++
				resolvedMarker = marker
				resolvedRevision = actual.GetSpecRevision()
			}
			if served == 0 {
				retainStressError(&lastErr, fmt.Errorf("GetService %s: no replica served owner-local state", expected.ID))
				return false, nil
			}
			if served > 1 {
				return false, fmt.Errorf("GetService %s was served by %d owner-gated replicas", expected.ID, served)
			}
			expected.Marker = resolvedMarker
			expected.Revision = resolvedRevision
			allocation, err := stressHealthyAllocation(ctx, o, clients, expected.ID, expected.Revision)
			if err != nil {
				retainStressError(&lastErr, fmt.Errorf("healthy allocation %s: %w", expected.ID, err))
				return false, nil
			}
			allocations[i] = allocation
		}
		probeErrors := make([]error, len(services))
		var probeWG sync.WaitGroup
		for i := range services {
			allocation, expected := allocations[i], services[i]
			host, ok := hosts[allocation.GetAgentId()]
			if !ok {
				return false, fmt.Errorf("unknown allocated agent %s", allocation.GetAgentId())
			}
			probeWG.Add(1)
			go func(i int) {
				defer probeWG.Done()
				callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
				defer cancel()
				probeErrors[i] = assertHTTPResponseInAllocationNetNS(callCtx, key, host.PublicIPv4, allocation.GetAllocationId(), allocationEndpoint(allocation), "/", expected.Marker)
			}(i)
		}
		probeWG.Wait()
		for i, err := range probeErrors {
			if err != nil {
				retainStressError(&lastErr, fmt.Errorf("service %s content: %w", services[i].ID, err))
				return false, nil
			}
		}
		// Verify cross-project isolation with a successful source-side control
		// and a positive target-side control, so denial cannot pass merely
		// because the target is unreachable.
		if len(services) >= 2 {
			source := allocations[0]
			sourceHost := hosts[source.GetAgentId()]
			sourceCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := assertHTTPResponseFromContainer(sourceCtx, key, sourceHost.PublicIPv4, managedContainerName(source.GetAllocationId()), allocationEndpoint(source), "/", services[0].Marker)
			cancel()
			if err != nil {
				retainStressError(&lastErr, err)
				return false, nil
			}
			for i := 1; i < len(services); i++ {
				if services[i].Environment != services[0].Environment {
					target := allocations[i]
					targetHost := hosts[target.GetAgentId()]
					targetCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
					err := assertHTTPResponseInAllocationNetNS(targetCtx, key, targetHost.PublicIPv4, target.GetAllocationId(), allocationEndpoint(target), "/", services[i].Marker)
					cancel()
					if err != nil {
						retainStressError(&lastErr, fmt.Errorf("isolation control target %s unreachable: %w", services[i].ID, err))
						return false, nil
					}
					denyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
					err = assertHTTPDeniedFromContainer(denyCtx, key, sourceHost.PublicIPv4, managedContainerName(source.GetAllocationId()), allocationEndpoint(target), "/")
					cancel()
					if err != nil {
						return false, fmt.Errorf("tenant isolation violated: %w", err)
					}
					break
				}
			}
		}
		return true, nil
	})
	if returnErr != nil {
		return errors.Join(returnErr, lastErr)
	}
	return nil
}

// verifyStressWatch validates blocking-watch invariants that do not require a
// concurrent mutation: the returned index never regresses, not_modified implies
// an unchanged index, and a watch never leaks services from another environment.
func verifyStressWatch(ctx context.Context, o stressOptions, clients []platformv1.PlatformServiceClient, services []stressService) error {
	environments := environmentSet(services)
	recoveryCtx, cancel := context.WithTimeout(ctx, o.Recovery)
	defer cancel()
	var lastErr error
	returnErr := testutil.Poll(recoveryCtx, testutil.PollConfig{Timeout: o.Recovery, Interval: time.Second}, func(ctx context.Context) (bool, error) {
		for _, environment := range environments {
			watched := false
			for _, client := range clients {
				callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
				baseline, err := client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: environment})
				cancel()
				if err != nil {
					if replicaRPCRedirected(err) {
						continue
					}
					retainStressError(&lastErr, fmt.Errorf("watch baseline ListServices env %s: %w", environment, err))
					return false, nil
				}
				callCtx, cancel = context.WithTimeout(ctx, 6*time.Second)
				watch, err := client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: environment, WaitIndex: baseline.GetIndex(), WaitTimeoutSeconds: 1})
				cancel()
				if err != nil {
					if replicaRPCRedirected(err) {
						continue
					}
					retainStressError(&lastErr, fmt.Errorf("watch ListServices env %s: %w", environment, err))
					return false, nil
				}
				if watch.GetIndex() < baseline.GetIndex() {
					return false, fmt.Errorf("watch index regressed from %d to %d", baseline.GetIndex(), watch.GetIndex())
				}
				if watch.GetNotModified() && watch.GetIndex() != baseline.GetIndex() {
					return false, fmt.Errorf("watch reported not_modified with index %d, baseline %d", watch.GetIndex(), baseline.GetIndex())
				}
				for _, service := range watch.GetServices() {
					if service.GetEnvironmentId() != environment {
						return false, fmt.Errorf("watch for %s returned service %s from environment %s", environment, service.GetId(), service.GetEnvironmentId())
					}
				}
				watched = true
				break
			}
			if !watched {
				retainStressError(&lastErr, fmt.Errorf("watch env %s: no replica served owner-local state", environment))
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

// watchDeliveryProbe writes one service and requires an owner-following
// blocking watch to observe the acknowledged revision.
func watchDeliveryProbe(ctx context.Context, o stressOptions, stage stressStage, rawClients []platformv1.PlatformServiceClient, mutationClient platformv1.PlatformServiceClient, services []stressService, index int) error {
	if index < 0 || index >= len(services) {
		return fmt.Errorf("watch probe service index %d is out of range", index)
	}
	service := &services[index]
	marker := fmt.Sprintf("seed-%d-service-%d-watch-%d-%d", o.Seed, service.Index, stage.Number, time.Now().UnixNano())
	var baseline int64
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: o.Recovery, Interval: time.Second}, func(ctx context.Context) (bool, error) {
		for _, client := range rawClients {
			callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
			response, err := client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: service.Environment})
			cancel()
			if err != nil {
				if replicaRPCRedirected(err) {
					continue
				}
				return false, nil
			}
			baseline = response.GetIndex()
			return true, nil
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("watch probe baseline: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
	updated, err := mutationClient.UpdateService(callCtx, &platformv1.UpdateServiceRequest{ServiceId: service.ID, Service: &platformv1.ServiceUpdate{Spec: service.spec(marker)}})
	cancel()
	if err != nil {
		return fmt.Errorf("watch probe update: %w", err)
	}
	err = testutil.Poll(ctx, testutil.PollConfig{Timeout: o.Recovery, Interval: time.Second}, func(ctx context.Context) (bool, error) {
		for _, client := range rawClients {
			callCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
			watch, err := client.ListServices(callCtx, &platformv1.ListServicesRequest{EnvironmentId: service.Environment, WaitIndex: baseline, WaitTimeoutSeconds: 2})
			cancel()
			if err != nil {
				if replicaRPCRedirected(err) {
					continue
				}
				return false, nil
			}
			if watch.GetIndex() < baseline {
				return false, fmt.Errorf("watch index regressed from %d to %d", baseline, watch.GetIndex())
			}
			if watch.GetNotModified() {
				return false, nil
			}
			for _, observed := range watch.GetServices() {
				if observed.GetId() == service.ID && observed.GetSpecRevision() >= updated.GetSpecRevision() {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("blocking watch did not observe acknowledged update: %w", err)
	}
	// When the update was staged rather than applied, discard it. Discarding
	// records a further durable revision, so the caller releases afterwards to
	// apply it before the next convergence check.
	if updated.GetPendingChanges() {
		callCtx, cancel = context.WithTimeout(ctx, o.RPCTimeout)
		_, err = mutationClient.DiscardServiceChanges(callCtx, &platformv1.DiscardServiceChangesRequest{ServiceId: service.ID, DiscardAll: true})
		cancel()
		if err != nil {
			return fmt.Errorf("discard watch probe change: %w", err)
		}
	}
	return nil
}

func captureStressResources(ctx context.Context, key string, hosts map[string]hostInfo, artifacts string, stage int) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// Cross-agent HTTP depends on the WireGuard data plane between the VMs'
	// advertised public addresses. Capture tunnel, routing, and namespace state
	// so a data-plane failure can be classified without another run.
	var peerPings []string
	for _, host := range hosts {
		if addr := strings.TrimSpace(host.PublicIPv4); addr != "" {
			peerPings = append(peerPings, "4 "+addr)
		}
		if addr := strings.TrimSpace(host.PublicIPv6); addr != "" {
			peerPings = append(peerPings, "6 "+trimCIDR(addr))
		}
	}
	sort.Strings(peerPings)
	var pingBuilder strings.Builder
	for _, target := range peerPings {
		family, addr, _ := strings.Cut(target, " ")
		fmt.Fprintf(&pingBuilder, "ping -%s -c 2 -W 2 %s 2>&1 | tail -2; ", family, addr)
	}
	meshProbe := "echo '=== MESH ==='; wg show all 2>&1; wg show all dump 2>&1; ss -lunp 2>/dev/null | grep -E '51820|wireguard'; " +
		"sysctl net.ipv4.ip_forward net.ipv6.conf.all.forwarding 2>/dev/null; ip -brief addr; ip route; ip -6 route; ip rule; " +
		"echo '=== NETNS ==='; for f in /var/lib/ebpf-wg-mesh/agent/netns/*.path; do [ -e \"$f\" ] || continue; ns=$(cat \"$f\"); " +
		"echo \"--- $(basename \"$f\" .path) ---\"; nsenter --net=\"$ns\" ip -brief addr 2>&1; nsenter --net=\"$ns\" ip route 2>&1; " +
		"nsenter --net=\"$ns\" ip -6 route 2>&1; nsenter --net=\"$ns\" nft list ruleset 2>&1 | head -120; done; echo '=== PEER PING ==='; " + pingBuilder.String()

	stateProbe := "echo '=== AGENTS ==='; cockroach sql --insecure --host=127.0.0.1:26257 --format=csv " +
		"--execute \"SELECT r.id, r.cpu_millis_capacity, r.reserved_cpu_millis, r.memory_mebibytes_capacity, r.reserved_memory_mebibytes, ad.lifecycle_state, r.desired_revision FROM agent_registrations r JOIN agent_administration ad ON ad.agent_id = r.id ORDER BY r.id\" 2>&1; " +
		"echo '=== USAGE ==='; cockroach sql --insecure --host=127.0.0.1:26257 --format=csv " +
		"--execute \"SELECT a.agent_id, count(*), COALESCE(sum((sr.spec_json->'runtime'->>'cpuMillis')::int8),0), COALESCE(sum((sr.spec_json->'runtime'->>'memoryMebibytes')::int8),0) FROM allocation_assignments a JOIN services s ON s.id = a.service_id LEFT JOIN service_revisions sr ON sr.service_id = a.service_id AND sr.spec_revision = s.current_spec_revision WHERE a.rollout_state <> 'lost' GROUP BY a.agent_id ORDER BY a.agent_id\" 2>&1; " +
		"echo '=== SERVICES ==='; cockroach sql --insecure --host=127.0.0.1:26257 --format=csv " +
		"--execute \"SELECT s.id, s.name, s.current_spec_revision, s.current_rollout_generation, s.placement_message FROM services s ORDER BY s.id\" 2>&1; " +
		"echo '=== ALLOCATIONS ==='; cockroach sql --insecure --host=127.0.0.1:26257 --format=csv " +
		"--execute \"SELECT a.service_id, a.id, a.agent_id, a.desired_spec_revision, a.desired_rollout_generation, a.rollout_state, a.intent, a.intent_message, a.allocation_ipv4, a.allocation_ipv6, a.created_at FROM allocation_assignments a ORDER BY a.service_id, a.created_at, a.id\" 2>&1; " +
		"echo '=== ROLLOUTS ==='; cockroach sql --insecure --host=127.0.0.1:26257 --format=csv " +
		"--execute \"SELECT service_id, rollout_generation, spec_revision, state, target_allocation_id, failure_reason, progress_at FROM service_rollouts ORDER BY service_id, rollout_generation\" 2>&1; " +
		"echo '=== DEPLOYMENTS ==='; cockroach sql --insecure --host=127.0.0.1:26257 --format=csv " +
		"--execute \"SELECT service_id, id, spec_revision, rollout_generation, state, cause_kind, reason_code, is_current, detail FROM deployments ORDER BY service_id, rollout_generation, created_at\" 2>&1; "

	for name, host := range hosts {
		command := "date -u; uptime; free -m; df -h; cat /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io; ss -s; " +
			"systemctl show ebpf-wg-mesh-controlplane ebpf-wg-mesh-controlplane-replica ebpf-wg-mesh-agent ebpf-wg-mesh-cockroach -p MemoryCurrent -p MemoryPeak -p CPUUsageNSec -p TasksCurrent -p NRestarts; " +
			"iptables-save; " + meshProbe + " dmesg --level=err,warn | tail -100; " +
			"journalctl -u ebpf-wg-mesh-controlplane -u ebpf-wg-mesh-controlplane-replica -u ebpf-wg-mesh-agent -u ebpf-wg-mesh-cockroach -n 300 --no-pager"
		if name == "controlplane" {
			command = stateProbe + command
		}
		out, err := runRemoteCommand(ctx, key, host.PublicIPv4, command)
		if err != nil {
			out = append(out, []byte("\nERROR: "+err.Error())...)
		}
		if err := os.WriteFile(filepath.Join(artifacts, fmt.Sprintf("stress-stage-%02d-%s.txt", stage, name)), out, 0600); err != nil {
			infof("stress resource snapshot: %v", err)
		}
	}
}

func stressOwnerService(ctx context.Context, key, host string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runRemoteCommand(ctx, key, host, "cockroach sql --insecure --host=127.0.0.1:26257 --format=csv --execute \"SELECT advertise_addr FROM control_plane_leases WHERE name = 'control-plane-singleton' AND expires_at > statement_timestamp();\"")
	if err != nil {
		return "", fmt.Errorf("read current owner: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == host+":"+primaryControlPlanePort {
			return primaryControlPlaneService, nil
		}
		if strings.TrimSpace(line) == host+":"+replicaControlPlanePort {
			return replicaControlPlaneService, nil
		}
	}
	return "", fmt.Errorf("no live owner in lease query: %s", out)
}

// Delete and recreate one seeded service at each load level. This exercises
// tombstones, reuse of names, allocation teardown, and increasing rollout churn.
func churnStressService(ctx context.Context, o stressOptions, stage stressStage, clients []platformv1.PlatformServiceClient, services []stressService, trace *stressTrace) error {
	index := stage.Updates[len(stage.Updates)-1]
	old := services[index]
	callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
	_, err := clients[0].DeleteService(callCtx, &platformv1.DeleteServiceRequest{ServiceId: old.ID})
	cancel()
	trace.record(stage.Number, "delete", map[string]any{"id": old.ID, "code": status.Code(err).String()})
	if err != nil {
		return fmt.Errorf("delete for churn: %w", err)
	}
	for _, client := range clients {
		waitCtx, cancel := context.WithTimeout(ctx, o.Recovery)
		err := waitForServiceDeletion(waitCtx, waitCtx, client, old.Environment, old.ID)
		cancel()
		if err != nil {
			return fmt.Errorf("deleted service remained visible: %w", err)
		}
		callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
		_, err = client.GetService(callCtx, &platformv1.GetServiceRequest{ServiceId: old.ID})
		cancel()
		if status.Code(err) != codes.NotFound {
			return fmt.Errorf("deleted service GetService returned %v; want NotFound", err)
		}
	}
	marker := fmt.Sprintf("seed-%d-service-%d-recreated-%d", o.Seed, index, stage.Number)
	callCtx, cancel = context.WithTimeout(ctx, o.RPCTimeout)
	created, err := clients[1].CreateService(callCtx, &platformv1.CreateServiceRequest{EnvironmentId: old.Environment, Service: &platformv1.ServiceInput{Name: "web-" + strconv.Itoa(index), Spec: old.spec(marker)}})
	cancel()
	trace.record(stage.Number, "recreate", map[string]any{"old_id": old.ID, "id": created.GetId(), "code": status.Code(err).String()})
	if err != nil {
		return fmt.Errorf("recreate service: %w", err)
	}
	if created.GetId() == old.ID || created.GetId() == "" {
		return errors.New("recreated service reused an old or empty identity")
	}
	services[index] = stressService{Index: old.Index, ID: created.GetId(), Environment: old.Environment, Marker: marker, Revision: created.GetSpecRevision()}
	return nil
}

func checkStressList(environment string, expected map[string]bool, actual *platformv1.ListServicesResponse) error {
	seen := make(map[string]bool)
	for _, service := range actual.GetServices() {
		if service.GetEnvironmentId() != environment || !expected[service.GetId()] {
			return fmt.Errorf("unexpected service %s in environment %s", service.GetId(), environment)
		}
		if seen[service.GetId()] {
			return fmt.Errorf("duplicate service %s in list", service.GetId())
		}
		seen[service.GetId()] = true
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("environment %s lists %d services, expected %d", environment, len(seen), len(expected))
	}
	return nil
}
