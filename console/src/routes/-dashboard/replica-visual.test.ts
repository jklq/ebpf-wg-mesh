import { describe, expect, it } from "vitest";

import type { DashboardAllocationStatus } from "#/lib/dashboard/core/types.server";

import {
	replicaAllocationsForDeployment,
	replicaPhaseLabel,
	replicaRolloutCopy,
	replicaSlots,
	shouldShowReplicaFleet,
} from "./replica-visual";

describe("replicaSlots", () => {
	it("fills missing desired replicas as pending and marks stragglers as rolling", () => {
		const slots = replicaSlots({
			desired: 4,
			desiredGeneration: 3,
			allocations: [
				allocation({
					allocationId: "a",
					healthy: true,
					appliedRolloutGeneration: 3,
					desiredRolloutGeneration: 3,
				}),
				allocation({
					allocationId: "b",
					healthy: false,
					appliedRolloutGeneration: 2,
					desiredRolloutGeneration: 3,
				}),
			],
		});

		expect(slots.map((slot) => slot.state)).toEqual([
			"ready",
			"rolling",
			"pending",
			"pending",
		]);
		expect(replicaRolloutCopy({ desired: 4, ready: 1, slots })).toBe(
			"Rolling 1 of 4 replicas",
		);
	});

	it("calls out crash-looped replicas instead of treating them as still rolling", () => {
		const slots = replicaSlots({
			desired: 2,
			desiredGeneration: 2,
			allocations: [
				allocation({
					allocationId: "a",
					healthy: true,
					appliedRolloutGeneration: 2,
					desiredRolloutGeneration: 2,
				}),
				allocation({
					allocationId: "b",
					healthy: false,
					appliedRolloutGeneration: 1,
					desiredRolloutGeneration: 2,
					restart: {
						restartCount: 8,
						crashLoop: true,
						lastCause: "oom",
						message: "crash loop",
					},
				}),
			],
		});

		expect(slots[1]?.state).toBe("failed");
		expect(replicaRolloutCopy({ desired: 2, ready: 1, slots })).toBe(
			"1 of 2 replicas failed",
		);
	});

	it("names live phases instead of a generic rolling label", () => {
		expect(
			replicaPhaseLabel(
				allocation({ phase: "ImagePull", healthy: false }),
				"rolling",
			),
		).toBe("Pulling image");
		expect(
			replicaPhaseLabel(
				allocation({ phase: "Starting", healthy: false }),
				"rolling",
			),
		).toBe("Starting");
	});

	it("scopes replicas to the deployment they belong to", () => {
		const allocations = [
			allocation({
				allocationId: "old",
				appliedRolloutGeneration: 1,
				desiredRolloutGeneration: 1,
			}),
			allocation({
				allocationId: "incoming",
				appliedRolloutGeneration: 1,
				desiredRolloutGeneration: 2,
			}),
			allocation({
				allocationId: "new",
				appliedRolloutGeneration: 2,
				desiredRolloutGeneration: 2,
			}),
		];
		expect(
			replicaAllocationsForDeployment({
				allocations,
				rolloutGeneration: 2,
				preferDesired: true,
			}).map((item) => item.allocationId),
		).toEqual(["incoming", "new"]);
		expect(
			replicaAllocationsForDeployment({
				allocations,
				rolloutGeneration: 1,
				preferDesired: false,
			}).map((item) => item.allocationId),
		).toEqual(["old"]);
	});

	it("marks overlapping predecessors as draining from rollout state", () => {
		const slots = replicaSlots({
			desired: 1,
			desiredGeneration: 2,
			allocations: [
				allocation({
					allocationId: "old",
					healthy: true,
					appliedRolloutGeneration: 1,
					desiredRolloutGeneration: 1,
					rolloutState: "draining",
					phase: "Draining",
				}),
				allocation({
					allocationId: "new",
					healthy: true,
					appliedRolloutGeneration: 2,
					desiredRolloutGeneration: 2,
					rolloutState: "serving",
				}),
			],
		});
		expect(slots.map((slot) => slot.state)).toEqual(["ready", "draining"]);
		expect(
			shouldShowReplicaFleet({
				desired: 1,
				allocationCount: 2,
				buildFailed: false,
				buildOpen: false,
			}),
		).toBe(true);
	});

	it("keeps a surge allocation starting when it sorts after the desired replicas", () => {
		const slots = replicaSlots({
			desired: 1,
			desiredGeneration: 2,
			allocations: [
				allocation({
					allocationId: "a-serving",
					healthy: true,
					appliedRolloutGeneration: 1,
					desiredRolloutGeneration: 1,
					rolloutState: "serving",
				}),
				allocation({
					allocationId: "z-surge",
					healthy: false,
					appliedRolloutGeneration: 1,
					desiredRolloutGeneration: 2,
					rolloutState: "starting",
					phase: "Starting",
				}),
			],
		});

		expect(slots.map((slot) => slot.state)).toEqual(["rolling", "rolling"]);
		expect(slots[1]?.phaseLabel).toBe("Starting");
	});

	it("orders desired, surge, and draining slots by rollout role instead of allocation id", () => {
		const slots = replicaSlots({
			desired: 1,
			desiredGeneration: 2,
			allocations: [
				allocation({
					allocationId: "a",
					healthy: false,
					appliedRolloutGeneration: 1,
					desiredRolloutGeneration: 2,
					rolloutState: "starting",
					phase: "Starting",
				}),
				allocation({
					allocationId: "m",
					appliedRolloutGeneration: 1,
					desiredRolloutGeneration: 1,
					rolloutState: "draining",
					phase: "Draining",
				}),
				allocation({
					allocationId: "z",
					appliedRolloutGeneration: 2,
					desiredRolloutGeneration: 2,
					rolloutState: "serving",
				}),
			],
		});

		expect(slots.map((slot) => slot.allocation?.allocationId)).toEqual([
			"z",
			"a",
			"m",
		]);
		expect(slots.map((slot) => slot.state)).toEqual([
			"ready",
			"rolling",
			"draining",
		]);
		expect(slots[0]?.label).toBe("Replica 1");
	});

	it("keeps a single-replica deploy on the shared stage list until a fleet exists", () => {
		expect(
			shouldShowReplicaFleet({
				desired: 1,
				allocationCount: 1,
				buildFailed: false,
				buildOpen: false,
			}),
		).toBe(false);
		expect(
			shouldShowReplicaFleet({
				desired: 3,
				allocationCount: 2,
				buildFailed: false,
				buildOpen: false,
			}),
		).toBe(true);
		expect(
			shouldShowReplicaFleet({
				desired: 3,
				allocationCount: 0,
				buildFailed: true,
				buildOpen: false,
			}),
		).toBe(false);
	});
});

function allocation(
	overrides: Partial<DashboardAllocationStatus>,
): DashboardAllocationStatus {
	return {
		allocationId: "alloc",
		serviceId: "service-1",
		agentId: "agent-1",
		desiredSpecRevision: 1,
		appliedSpecRevision: 1,
		phase: "Running",
		message: "",
		allocationIp: "",
		healthy: true,
		desiredRolloutGeneration: 1,
		appliedRolloutGeneration: 1,
		healthyPorts: [8080],
		...overrides,
	};
}
