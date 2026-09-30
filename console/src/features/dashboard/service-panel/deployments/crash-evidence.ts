import type { DashboardAllocationStatus } from "#/lib/dashboard/core/types.server";

export type CrashCause =
	| "oom"
	| "disk"
	| "liveness"
	| "nonzero-exit"
	| "signal"
	| "exit-zero"
	| "operator"
	| "node-loss"
	| "probe-not-ready"
	| "none";

export function crashCauseForAllocation(
	allocation: DashboardAllocationStatus,
): CrashCause {
	const cause = allocation.restart?.lastCause ?? "";
	switch (cause) {
		case "RESTART_CAUSE_OOM_KILL":
			return "oom";
		case "RESTART_CAUSE_DISK_EXHAUSTED":
			return "disk";
		case "RESTART_CAUSE_LIVENESS":
			return "liveness";
		case "RESTART_CAUSE_EXIT_NONZERO":
			return "nonzero-exit";
		case "RESTART_CAUSE_SIGNAL":
			return "signal";
		case "RESTART_CAUSE_EXIT_ZERO":
			return "exit-zero";
		case "RESTART_CAUSE_OPERATOR":
			return "operator";
		case "RESTART_CAUSE_NODE_LOSS":
			return "node-loss";
		default:
			break;
	}
	if (isProbeNotReady(allocation)) {
		return "probe-not-ready";
	}
	return "none";
}

export function crashCauseLabel(cause: CrashCause): string {
	switch (cause) {
		case "oom":
			return "OOM kill";
		case "disk":
			return "Disk exhausted";
		case "liveness":
			return "Liveness restart";
		case "nonzero-exit":
			return "Non-zero exit";
		case "signal":
			return "Signal";
		case "exit-zero":
			return "Exited";
		case "operator":
			return "Operator restart";
		case "node-loss":
			return "Node loss";
		case "probe-not-ready":
			return "Probe not ready";
		case "none":
			return "No crash";
	}
}

export function hasCrashEvidence(
	allocation: DashboardAllocationStatus,
): boolean {
	if (allocation.restart?.crashLoop) return true;
	if ((allocation.restart?.restartCount ?? 0) > 0) return true;
	const cause = crashCauseForAllocation(allocation);
	if (cause !== "none" && cause !== "probe-not-ready") return true;
	if (allocation.phase === "CrashLoop") return true;
	if (allocation.phase === "Backoff") return true;
	if (allocation.phase === "Stopped") return true;
	if (
		allocation.phase === "Error" ||
		allocation.phase === "Failed" ||
		allocation.phase === "Unhealthy"
	) {
		return true;
	}
	return isProbeNotReady(allocation);
}

export function isProbeNotReady(
	allocation: DashboardAllocationStatus,
): boolean {
	if (allocation.healthy) return false;
	if (allocation.restart?.crashLoop) return false;
	const cause = allocation.restart?.lastCause ?? "";
	if (
		cause === "RESTART_CAUSE_OOM_KILL" ||
		cause === "RESTART_CAUSE_DISK_EXHAUSTED" ||
		cause === "RESTART_CAUSE_LIVENESS" ||
		cause === "RESTART_CAUSE_EXIT_NONZERO" ||
		cause === "RESTART_CAUSE_SIGNAL"
	) {
		return false;
	}
	if (allocation.phase === "Starting") return true;
	return allocation.message.toLowerCase().includes("readiness check not ready");
}

export function formatCrashSummary(
	allocation: DashboardAllocationStatus,
): string {
	const parts: string[] = [];
	const cause = crashCauseForAllocation(allocation);
	if (cause !== "none") {
		parts.push(crashCauseLabel(cause));
	}
	const restart = allocation.restart;
	if (restart) {
		if (
			cause === "nonzero-exit" ||
			cause === "oom" ||
			restart.lastExitCode !== 0
		) {
			parts.push(`exit ${restart.lastExitCode}`);
		}
		if (restart.lastSignal !== 0) {
			parts.push(`signal ${restart.lastSignal}`);
		}
		if (restart.restartCount > 0) {
			parts.push(
				`${restart.restartCount} ${restart.restartCount === 1 ? "restart" : "restarts"}`,
			);
		}
		if (restart.crashLoop) {
			parts.push("crash loop");
		} else if (restart.awaitingRestart) {
			parts.push("backoff");
		}
	}
	if (parts.length === 0) {
		return allocation.message || allocation.phase || "No crash evidence";
	}
	return parts.join(" · ");
}
