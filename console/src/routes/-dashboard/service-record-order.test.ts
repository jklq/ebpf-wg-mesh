import { describe, expect, it } from "vitest";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

import { newestServiceRecord } from "./service-record-order";

describe("newestServiceRecord", () => {
	it("rejects a late snapshot with an older spec revision", () => {
		const current = service({ specRevision: 4, name: "current" });
		const stale = service({ specRevision: 3, name: "stale" });

		expect(newestServiceRecord(current, stale)).toBe(current);
	});

	it("orders equal specs by rollout generation and update time", () => {
		const current = service({
			specRevision: 4,
			rolloutGeneration: 2,
			updatedAt: new Date("2026-01-01T00:00:00Z"),
		});
		const newerRollout = service({
			specRevision: 4,
			rolloutGeneration: 3,
			updatedAt: new Date("2025-01-01T00:00:00Z"),
		});
		const newerDecoration = service({
			specRevision: 4,
			rolloutGeneration: 3,
			updatedAt: new Date("2026-02-01T00:00:00Z"),
		});

		expect(newestServiceRecord(current, newerRollout)).toBe(newerRollout);
		expect(newestServiceRecord(newerRollout, newerDecoration)).toBe(
			newerDecoration,
		);
	});
});

function service(
	overrides: Partial<DashboardServiceRecord>,
): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		name: "web",
		...overrides,
	};
}
