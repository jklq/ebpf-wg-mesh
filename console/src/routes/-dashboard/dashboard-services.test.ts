import { describe, expect, it } from "vitest";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import {
	applyLoaderState,
	applyMutationRecord,
	applyMutationStatus,
	applyServiceStatusSnapshot,
	applyServicesSnapshot,
	createNormalizedState,
	removeService,
	selectSelectedStatus,
	selectServicesArray,
	upsertServiceRecord,
} from "./dashboard-services";

describe("dashboard-services revision contract", () => {
	it("ignores list snapshots at or below the applied revision", () => {
		const state = createNormalizedState(loader([record("service-1")], 2));
		const stale = applyServicesSnapshot(state, {
			services: [record("service-1", { name: "stale" })],
			revision: 2,
		});
		expect(stale).toBe(state);
		const older = applyServicesSnapshot(state, {
			services: [],
			revision: 1,
		});
		expect(older).toBe(state);
		const newer = applyServicesSnapshot(state, {
			services: [record("service-1", { name: "fresh" })],
			revision: 3,
		});
		expect(newer.servicesById["service-1"]?.name).toBe("fresh");
		expect(newer.servicesRevision).toBe(3);
	});

	it("keeps a stale loader from clearing newer snapshot data", () => {
		const dirty = record("service-1", {
			pendingChanges: true,
			unappliedChangeCount: 1,
		});
		const state = createNormalizedState(loader([dirty], 2));
		const next = applyLoaderState(state, loader([], 1));
		expect(selectServicesArray(next)).toHaveLength(1);
		expect(next.servicesRevision).toBe(2);
	});

	it("accepts a newer loader snapshot", () => {
		const state = createNormalizedState(loader([record("service-1")], 1));
		const next = applyLoaderState(
			state,
			loader([record("service-1", { name: "renamed" })], 2),
		);
		expect(next.servicesById["service-1"]?.name).toBe("renamed");
	});

	it("resets scope on environment switch regardless of revision", () => {
		const state = createNormalizedState(loader([record("service-1")], 9));
		const next = applyLoaderState(
			state,
			loader([record("service-2")], 1, "environment-2"),
		);
		expect(selectServicesArray(next).map((s) => s.id)).toEqual(["service-2"]);
		expect(next.servicesRevision).toBe(1);
	});

	it("keeps optimistic selection when an empty loader follows a create", () => {
		const created = record("service-9");
		const state = createNormalizedState(loader([], 1), {
			selectedServiceId: null,
		});
		const optimistic = {
			...upsertServiceRecord(state, created),
			selectedServiceId: "service-9" as string | null,
			base: {
				...state.base,
				environment: {
					id: "environment-1",
					projectId: "project-1",
					name: "Production",
					kind: "persistent" as const,
					isProduction: true,
					autoDeploy: false,
				},
			},
		};
		const next = applyLoaderState(
			optimistic,
			{ ...loader([], 1), environment: undefined },
			[created],
		);
		expect(selectServicesArray(next).map((s) => s.id)).toEqual(["service-9"]);
		expect(next.selectedServiceId).toBe("service-9");
	});

	it("retains created services until the snapshot includes them", () => {
		const created = record("service-9");
		const state = createNormalizedState(loader([], 1));
		const next = applyServicesSnapshot(state, { services: [], revision: 2 }, [
			created,
		]);
		expect(selectServicesArray(next).map((s) => s.id)).toEqual(["service-9"]);
		const converged = applyServicesSnapshot(
			next,
			{ services: [created], revision: 3 },
			[],
		);
		expect(selectServicesArray(converged).map((s) => s.id)).toEqual([
			"service-9",
		]);
	});

	it("gates status snapshots per service", () => {
		const state = createNormalizedState(
			loader([record("service-1"), record("service-2")], 1),
		);
		const withStatus = applyServiceStatusSnapshot(state, {
			status: { service: record("service-1"), allocation: undefined },
			revision: 5,
		});
		const stale = applyServiceStatusSnapshot(withStatus, {
			status: {
				service: record("service-1", { name: "stale" }),
				allocation: undefined,
			},
			revision: 4,
		});
		expect(stale.servicesById["service-1"]?.name).toBe("web");
		const other = applyServiceStatusSnapshot(withStatus, {
			status: { service: record("service-2"), allocation: undefined },
			revision: 1,
		});
		expect(other.servicesById["service-2"]?.id).toBe("service-2");
	});

	it("ignores mutation responses dispatched before a newer snapshot", () => {
		const state = createNormalizedState(loader([record("service-1")], 1));
		const advanced = applyServicesSnapshot(state, {
			services: [record("service-1", { name: "newer" })],
			revision: 2,
		});
		const staleDeploy = applyMutationStatus(
			advanced,
			{ service: record("service-1", { name: "deploy-response" }) },
			1,
		);
		expect(staleDeploy.servicesById["service-1"]?.name).toBe("newer");
		const freshDeploy = applyMutationStatus(
			advanced,
			{ service: record("service-1", { name: "deploy-response" }) },
			2,
		);
		expect(freshDeploy.servicesById["service-1"]?.name).toBe("deploy-response");
		const staleDiscard = applyMutationRecord(
			advanced,
			record("service-1", { name: "discard-response" }),
			1,
		);
		expect(staleDiscard.servicesById["service-1"]?.name).toBe("newer");
	});

	it("stores each service once and reconstructs status without duplication", () => {
		const selected = createNormalizedState(loader([record("service-1")], 1), {
			selectedServiceId: "service-1",
		});
		const withStatus = applyServiceStatusSnapshot(selected, {
			status: {
				service: record("service-1"),
				allocation: {
					allocationId: "allocation-1",
					serviceId: "service-1",
					agentId: "agent-1",
					desiredSpecRevision: 1,
					appliedSpecRevision: 1,
					phase: "Running",
					message: "",
					allocationIpv4: "",
					allocationIpv6: "",
					healthy: true,
					desiredRolloutGeneration: 1,
					appliedRolloutGeneration: 1,
					healthyIpv4Ports: [],
					healthyIpv6Ports: [],
				},
			},
			revision: 1,
		});
		const status = selectSelectedStatus(withStatus);
		expect(status?.service).toBe(withStatus.servicesById["service-1"]);
		expect(status?.allocation?.allocationId).toBe("allocation-1");
	});

	it("upserts single records causally and removes deletes", () => {
		const state = createNormalizedState(loader([], 1));
		const created = upsertServiceRecord(state, record("service-1"));
		expect(selectServicesArray(created).map((s) => s.id)).toEqual([
			"service-1",
		]);
		const renamed = upsertServiceRecord(
			created,
			record("service-1", { name: "renamed" }),
		);
		expect(renamed.serviceOrder).toEqual(["service-1"]);
		expect(renamed.servicesById["service-1"]?.name).toBe("renamed");
		const removed = removeService(renamed, "service-1");
		expect(selectServicesArray(removed)).toEqual([]);
	});
});

function record(
	id: string,
	overrides: Partial<DashboardServiceRecord> = {},
): DashboardServiceRecord {
	return {
		id,
		environmentId: "environment-1",
		name: "web",
		...overrides,
	};
}

function loader(
	services: Array<DashboardServiceRecord>,
	servicesRevision: number,
	environmentId = "environment-1",
): DashboardHomeState {
	return {
		user: { id: "user-1", email: "user@example.com" },
		onboarding: {
			projectId: "project-1",
			environmentId,
			serviceId: "",
			repositorySelector: "",
			trackedRef: "main",
			builder: "BUILDER_KIND_DOCKERFILE",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		publicBaseURL: "https://dashboard.example.test",
		ingressTargetHost: "platform.example.test",
		environments: [],
		environment: {
			id: environmentId,
			projectId: "project-1",
			name: "Production",
			kind: "persistent",
			isProduction: true,
			autoDeploy: false,
		},
		services,
		servicesRevision,
		selectedServiceId: null,
		domainBindings: [],
		controlPlaneReachable: true,
	};
}
