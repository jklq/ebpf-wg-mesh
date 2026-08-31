import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

/**
 * Service data arrives independently from route loaders, two event streams,
 * and mutation responses. Never let an older snapshot replace a newer spec or
 * rollout merely because it happened to arrive last.
 */
export function newestServiceRecord(
	current: DashboardServiceRecord | undefined,
	incoming: DashboardServiceRecord,
): DashboardServiceRecord {
	if (!current) return incoming;

	const revisionOrder = compareOptionalNumber(
		current.specRevision,
		incoming.specRevision,
	);
	if (revisionOrder !== 0) return revisionOrder < 0 ? incoming : current;

	const rolloutOrder = compareOptionalNumber(
		current.rolloutGeneration,
		incoming.rolloutGeneration,
	);
	if (rolloutOrder !== 0) return rolloutOrder < 0 ? incoming : current;

	const updatedOrder = compareOptionalNumber(
		current.updatedAt?.getTime(),
		incoming.updatedAt?.getTime(),
	);
	if (updatedOrder !== 0) return updatedOrder < 0 ? incoming : current;

	// Equal-version snapshots may contain fresher decoration (build, allocation,
	// or unapplied-change data), so the latest arrival wins once authority is tied.
	return incoming;
}

function compareOptionalNumber(
	left: number | undefined,
	right: number | undefined,
): number {
	if (left === right) return 0;
	if (left === undefined) return -1;
	if (right === undefined) return 1;
	return left - right;
}
