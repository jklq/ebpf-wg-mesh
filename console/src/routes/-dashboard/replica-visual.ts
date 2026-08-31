import type { DashboardAllocationStatus } from "#/lib/dashboard/core/types.server";

export type ReplicaSlotState =
	| "ready"
	| "rolling"
	| "failed"
	| "pending"
	| "draining";

export type ReplicaSlot = {
	id: string;
	index: number;
	state: ReplicaSlotState;
	title: string;
	label: string;
	phaseLabel: string;
	detail?: string;
	allocation?: DashboardAllocationStatus;
};

const ROLLOUT_STAGE_KEYS = new Set([
	"deploy",
	"post-deploy",
	"rollout",
	"readiness",
]);

export function isRolloutStage(stage: { key?: string }): boolean {
	return Boolean(stage.key && ROLLOUT_STAGE_KEYS.has(stage.key));
}

export function shouldShowReplicaFleet({
	desired,
	allocationCount,
	buildFailed,
	buildOpen,
}: {
	desired: number;
	allocationCount: number;
	buildFailed: boolean;
	buildOpen: boolean;
}): boolean {
	if (buildFailed) return false;
	if (desired <= 1 && allocationCount <= 1) return false;
	return !buildOpen || allocationCount > 1;
}

export function replicaAllocationsForDeployment({
	allocations,
	rolloutGeneration,
	preferDesired,
}: {
	allocations: Array<DashboardAllocationStatus>;
	rolloutGeneration?: number;
	preferDesired: boolean;
}): Array<DashboardAllocationStatus> {
	if (!rolloutGeneration) return [];
	if (preferDesired) {
		return allocations.filter(
			(allocation) => allocation.desiredRolloutGeneration === rolloutGeneration,
		);
	}
	return allocations.filter(
		(allocation) =>
			allocation.appliedRolloutGeneration === rolloutGeneration &&
			allocation.desiredRolloutGeneration === rolloutGeneration,
	);
}

export function replicaSlots({
	desired,
	allocations,
	desiredGeneration,
}: {
	desired: number;
	allocations: Array<DashboardAllocationStatus>;
	desiredGeneration?: number;
}): ReplicaSlot[] {
	const sorted = [...allocations].sort((left, right) =>
		left.allocationId.localeCompare(right.allocationId),
	);
	const count = Math.max(desired, sorted.length, 0);
	const slots: ReplicaSlot[] = [];
	for (let index = 0; index < count; index++) {
		const allocation = sorted[index];
		if (!allocation) {
			slots.push({
				id: `pending-${index}`,
				index,
				state: "pending",
				title: `Replica ${index + 1} · starting`,
				label: `Replica ${index + 1}`,
				phaseLabel: "Waiting for a slot",
			});
			continue;
		}
		const state = replicaAllocationState(allocation, desiredGeneration);
		slots.push({
			id: allocation.allocationId || `replica-${index}`,
			index,
			state,
			title: replicaSlotTitle(index, state, allocation),
			label: `Replica ${index + 1}`,
			phaseLabel: replicaPhaseLabel(allocation, state),
			detail: replicaSlotDetail(allocation, state),
			allocation,
		});
	}
	return slots;
}

export function replicaRolloutCopy({
	desired,
	ready,
	slots,
}: {
	desired: number;
	ready: number;
	slots: ReplicaSlot[];
}): string {
	const rolling = slots.filter(
		(slot) => slot.state === "rolling" || slot.state === "pending",
	).length;
	const failed = slots.filter((slot) => slot.state === "failed").length;
	const draining = slots.filter((slot) => slot.state === "draining").length;
	if (desired === 0) {
		return draining > 0 ? `Draining ${draining} replicas` : "Scaled to zero";
	}
	if (failed > 0) {
		return `${failed} of ${desired} replicas failed`;
	}
	if (rolling > 0) {
		return `Rolling ${ready} of ${desired} replicas`;
	}
	return `${ready} of ${desired} ready`;
}

function replicaAllocationState(
	allocation: DashboardAllocationStatus,
	desiredGeneration: number | undefined,
): ReplicaSlotState {
	if (allocation.restart?.crashLoop) {
		return "failed";
	}
	const phase = allocation.phase.toLowerCase();
	if (
		phase.includes("fail") ||
		phase.includes("crash") ||
		phase.includes("error")
	) {
		return "failed";
	}
	const rolloutState = allocation.rolloutState?.toLowerCase();
	if (rolloutState === "draining" || rolloutState === "withdrawing") {
		return "draining";
	}
	if (rolloutState === "starting") {
		return "rolling";
	}
	const behindGeneration =
		desiredGeneration !== undefined &&
		allocation.appliedRolloutGeneration < desiredGeneration;
	if (behindGeneration || !allocation.healthy) {
		return "rolling";
	}
	return "ready";
}

function replicaSlotTitle(
	index: number,
	state: ReplicaSlotState,
	allocation: DashboardAllocationStatus,
): string {
	const label = `Replica ${index + 1}`;
	return `${label} · ${replicaPhaseLabel(allocation, state)}`;
}

export function replicaPhaseLabel(
	allocation: DashboardAllocationStatus | undefined,
	state: ReplicaSlotState,
): string {
	if (!allocation) {
		return state === "pending" ? "Waiting for a slot" : "Starting";
	}
	if (state === "failed") {
		return "Failed";
	}
	if (state === "draining") {
		return "Draining";
	}
	if (state === "ready") {
		return allocation.healthyPorts.length > 0
			? `Healthy · :${allocation.healthyPorts.join(",:")}`
			: "Healthy";
	}
	const phase = allocation.phase.trim().toLowerCase();
	if (phase.includes("pull")) return "Pulling image";
	if (phase.includes("schedul")) return "Scheduling";
	if (phase.includes("start")) return "Starting";
	if (phase.includes("read")) return "Checking health";
	if (phase.includes("pend") || phase.includes("wait")) return "Pending";
	if (phase.includes("run") || phase.includes("active")) return "Coming up";
	if (allocation.phase.trim()) return allocation.phase.trim();
	return "Rolling";
}

function replicaSlotDetail(
	allocation: DashboardAllocationStatus,
	state: ReplicaSlotState,
): string | undefined {
	if (state === "failed") {
		return (
			allocation.restart?.message ||
			allocation.message ||
			allocation.restart?.lastCause ||
			undefined
		);
	}
	if (allocation.message.trim()) {
		return allocation.message.trim();
	}
	if (
		allocation.appliedRolloutGeneration !== allocation.desiredRolloutGeneration
	) {
		return `generation ${allocation.appliedRolloutGeneration} → ${allocation.desiredRolloutGeneration}`;
	}
	return undefined;
}
