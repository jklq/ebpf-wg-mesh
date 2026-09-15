import type {
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import type {
	ServiceStatusSnapshot,
	ServicesSnapshot,
} from "./dashboard-services";

export function hydrateServiceStatusSnapshot(
	raw: unknown,
): DashboardServiceStatus {
	const snapshot = raw as DashboardServiceStatus & {
		service: DashboardServiceStatus["service"] & {
			latestBuild?: DashboardServiceStatus["service"]["latestBuild"];
		};
	};
	return {
		...snapshot,
		service: hydrateServiceSnapshot(snapshot.service),
		allocation: hydrateAllocation(snapshot.allocation),
		allocations: Array.isArray(snapshot.allocations)
			? snapshot.allocations
					.map(hydrateAllocation)
					.filter((entry): entry is NonNullable<typeof entry> => Boolean(entry))
			: undefined,
	};
}

function hydrateAllocation(
	allocation: DashboardServiceStatus["allocation"],
): DashboardServiceStatus["allocation"] {
	if (!allocation) return undefined;
	return {
		...allocation,
		updatedAt: hydrateDate(allocation.updatedAt),
		drainStartedAt: hydrateDate(allocation.drainStartedAt),
		drainDeadline: hydrateDate(allocation.drainDeadline),
		restart: allocation.restart
			? {
					...allocation.restart,
					windowStartedAt: hydrateDate(allocation.restart.windowStartedAt),
					lastRestartAt: hydrateDate(allocation.restart.lastRestartAt),
					nextRestartAt: hydrateDate(allocation.restart.nextRestartAt),
					startedAt: hydrateDate(allocation.restart.startedAt),
				}
			: undefined,
	};
}

export function hydrateServiceSnapshots(
	raw: unknown,
): Array<DashboardServiceRecord> {
	return Array.isArray(raw) ? raw.map(hydrateServiceSnapshot) : [];
}

export function hydrateServicesSnapshot(raw: unknown): ServicesSnapshot {
	if (Array.isArray(raw)) {
		return { services: raw.map(hydrateServiceSnapshot), revision: 0 };
	}
	const snapshot = raw as {
		services?: unknown;
		revision?: unknown;
		index?: unknown;
	};
	return {
		services: hydrateServiceSnapshots(snapshot.services),
		revision: asRevision(snapshot.revision ?? snapshot.index),
	};
}

export function hydrateStatusSnapshot(raw: unknown): ServiceStatusSnapshot {
	const snapshot = raw as {
		status?: unknown;
		revision?: unknown;
		index?: unknown;
		service?: unknown;
	};
	if (snapshot && typeof snapshot === "object" && "service" in snapshot) {
		return {
			status: hydrateServiceStatusSnapshot(raw),
			revision: asRevision(snapshot.revision ?? snapshot.index),
		};
	}
	return {
		status: hydrateServiceStatusSnapshot(
			(snapshot as { status?: unknown }).status,
		),
		revision: asRevision(snapshot.revision ?? snapshot.index),
	};
}

export function hydrateServiceSnapshot(
	service: DashboardServiceRecord,
): DashboardServiceRecord {
	return {
		...service,
		createdAt: hydrateDate(service.createdAt),
		updatedAt: hydrateDate(service.updatedAt),
		latestBuild: service.latestBuild
			? {
					...service.latestBuild,
					queuedAt: hydrateDate(service.latestBuild.queuedAt),
					startedAt: hydrateDate(service.latestBuild.startedAt),
					finishedAt: hydrateDate(service.latestBuild.finishedAt),
					stages:
						service.latestBuild.stages?.map((stage) => ({
							...stage,
							startedAt: hydrateDate(stage.startedAt),
							finishedAt: hydrateDate(stage.finishedAt),
						})) ?? [],
				}
			: undefined,
		latestDeployment: service.latestDeployment
			? {
					...service.latestDeployment,
					transitionedAt: hydrateDate(service.latestDeployment.transitionedAt),
				}
			: undefined,
	};
}

function asRevision(value: unknown): number {
	return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function hydrateDate(value: Date | string | undefined): Date | undefined {
	if (!value) return undefined;
	return value instanceof Date ? value : new Date(value);
}
