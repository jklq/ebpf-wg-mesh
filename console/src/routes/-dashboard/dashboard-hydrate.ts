import type {
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

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

function hydrateDate(value: Date | string | undefined): Date | undefined {
	if (!value) return undefined;
	return value instanceof Date ? value : new Date(value);
}
