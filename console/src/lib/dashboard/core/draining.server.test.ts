import { describe, expect, it } from "vitest";

import { isDraining } from "#/lib/dashboard/core/draining.server";

describe("isDraining", () => {
	const env = { DASHBOARD_DRAIN_FILE: "/state/draining", INVOCATION_ID: "a1" };

	it("withdraws only the invocation named by the stop preparation", () => {
		expect(isDraining(env, () => "a1\n")).toBe(true);
		expect(isDraining(env, () => "previous-invocation\n")).toBe(false);
	});

	it("serves when no marker exists or the process is not supervised", () => {
		expect(
			isDraining(env, () => {
				throw new Error("ENOENT");
			}),
		).toBe(false);
		expect(
			isDraining({ DASHBOARD_DRAIN_FILE: "/state/draining" }, () => ""),
		).toBe(false);
		expect(isDraining({ INVOCATION_ID: "a1" }, () => "a1")).toBe(false);
	});
});
