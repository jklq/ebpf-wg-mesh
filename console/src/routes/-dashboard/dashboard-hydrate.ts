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
		allocation: snapshot.allocation
			? {
					...snapshot.allocation,
					updatedAt: hydrateDate(snapshot.allocation.updatedAt),
				}
			: undefined,
	};
}

export function hydrateServiceSnapshots(
	raw: unknown,
): Array<DashboardServiceRecord> {
	return Array.isArray(raw) ? raw.map(hydrateServiceSnapshot) : [];
}

/**
 * Revisioned list payload: `{ services, revision }`. Accepts a bare array
 * for tolerance and treats it as revision 0 (older than any real snapshot).
 */
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

/**
 * Revisioned status payload: `{ status, revision }`. Accepts a bare status
 * for tolerance and treats it as revision 0.
 */
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
