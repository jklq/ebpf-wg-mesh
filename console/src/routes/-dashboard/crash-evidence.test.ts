import { describe, expect, it } from "vitest";
import type { DashboardAllocationStatus } from "#/lib/dashboard/core/types.server";
import {
	crashCauseForAllocation,
	crashCauseLabel,
	formatCrashSummary,
	hasCrashEvidence,
	isProbeNotReady,
} from "./crash-evidence";

function allocation(
	overrides: Partial<DashboardAllocationStatus> = {},
): DashboardAllocationStatus {
	return {
		allocationId: "alloc-1",
		serviceId: "svc-1",
		agentId: "agent-1",
		desiredSpecRevision: 1,
		appliedSpecRevision: 1,
		phase: "Healthy",
		message: "",
		allocationIpv4: "10.200.0.2",
		allocationIpv6: "fd00:200::2",
		healthy: true,
		desiredRolloutGeneration: 1,
		appliedRolloutGeneration: 1,
		healthyIpv4Ports: [8080],
		healthyIpv6Ports: [8080],
		...overrides,
	};
}

describe("crash evidence classification", () => {
	it("distinguishes OOM, liveness, non-zero exit, and probe-not-ready", () => {
		const oom = allocation({
			healthy: false,
			phase: "CrashLoop",
			restart: {
				restartCount: 5,
				crashLoop: true,
				lastCause: "RESTART_CAUSE_OOM_KILL",
				message: "crash loop after OOM kill",
				lastExitCode: 137,
				lastSignal: 0,
				awaitingRestart: false,
			},
		});
		const liveness = allocation({
			healthy: false,
			phase: "Backoff",
			restart: {
				restartCount: 2,
				crashLoop: false,
				lastCause: "RESTART_CAUSE_LIVENESS",
				message: "restarting after liveness restart",
				lastExitCode: 0,
				lastSignal: 0,
				awaitingRestart: true,
			},
		});
		const nonzero = allocation({
			healthy: false,
			phase: "Backoff",
			restart: {
				restartCount: 1,
				crashLoop: false,
				lastCause: "RESTART_CAUSE_EXIT_NONZERO",
				message: "restarting after non-zero exit",
				lastExitCode: 1,
				lastSignal: 0,
				awaitingRestart: true,
			},
		});
		const notReady = allocation({
			healthy: false,
			phase: "Starting",
			message: "HTTP readiness check not ready: IPv4: connection refused",
		});

		expect(crashCauseForAllocation(oom)).toBe("oom");
		expect(crashCauseForAllocation(liveness)).toBe("liveness");
		expect(crashCauseForAllocation(nonzero)).toBe("nonzero-exit");
		expect(crashCauseForAllocation(notReady)).toBe("probe-not-ready");
		expect(crashCauseLabel("oom")).toBe("OOM kill");
		expect(crashCauseLabel("liveness")).toBe("Liveness restart");
		expect(crashCauseLabel("nonzero-exit")).toBe("Non-zero exit");
		expect(crashCauseLabel("probe-not-ready")).toBe("Probe not ready");
	});

	it("keeps probe-not-ready separate from process death", () => {
		const notReady = allocation({
			healthy: false,
			phase: "Starting",
			message: "HTTP readiness check not ready",
		});
		expect(isProbeNotReady(notReady)).toBe(true);
		expect(hasCrashEvidence(notReady)).toBe(true);

		const oom = allocation({
			healthy: false,
			phase: "Starting",
			message: "HTTP readiness check not ready",
			restart: {
				restartCount: 1,
				crashLoop: false,
				lastCause: "RESTART_CAUSE_OOM_KILL",
				message: "OOM kill",
				lastExitCode: 137,
				lastSignal: 0,
				awaitingRestart: true,
			},
		});
		expect(isProbeNotReady(oom)).toBe(false);
		expect(crashCauseForAllocation(oom)).toBe("oom");
	});

	it("formats exit code, signal, restart count, and crash loop", () => {
		const entry = allocation({
			healthy: false,
			phase: "CrashLoop",
			restart: {
				restartCount: 5,
				crashLoop: true,
				lastCause: "RESTART_CAUSE_OOM_KILL",
				message: "crash loop",
				lastExitCode: 137,
				lastSignal: 9,
				awaitingRestart: false,
			},
		});
		const summary = formatCrashSummary(entry);
		expect(summary).toContain("OOM kill");
		expect(summary).toContain("exit 137");
		expect(summary).toContain("signal 9");
		expect(summary).toContain("5 restarts");
		expect(summary).toContain("crash loop");
	});

	it("ignores healthy allocations without restart history", () => {
		expect(hasCrashEvidence(allocation())).toBe(false);
	});
});
