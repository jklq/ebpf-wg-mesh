import { describe, expect, it } from "vitest";

import { cleanDate, formatRelativeTime, formatTimeUntil } from "#/lib/time";

describe("cleanDate", () => {
	it("keeps missing dates absent", () => {
		expect(cleanDate(undefined)).toBeUndefined();
		expect(cleanDate(null)).toBeUndefined();
		expect(cleanDate("")).toBeUndefined();
	});

	it("keeps epoch values absent", () => {
		expect(cleanDate(new Date(0))).toBeUndefined();
		expect(cleanDate("1970-01-01T00:00:00.000Z")).toBeUndefined();
		expect(cleanDate(0)).toBeUndefined();
	});

	it("keeps malformed values absent", () => {
		expect(cleanDate(new Date("not a date"))).toBeUndefined();
		expect(cleanDate("not a date")).toBeUndefined();
		expect(cleanDate({})).toBeUndefined();
	});

	it("passes future and valid dates through", () => {
		const future = new Date(Date.now() + 3_600_000);
		expect(cleanDate(future)).toBe(future);
		expect(cleanDate("2026-08-13T10:00:22.000Z")).toEqual(
			new Date("2026-08-13T10:00:22.000Z"),
		);
	});
});

describe("formatRelativeTime", () => {
	const nowMs = new Date("2026-08-13T10:05:00.000Z").getTime();

	it("labels missing and epoch times instead of measuring from 1970", () => {
		expect(formatRelativeTime(undefined, nowMs)).toBe("Not deployed");
		expect(formatRelativeTime(new Date(0), nowMs)).toBe("Not deployed");
		expect(formatRelativeTime(new Date("bad"), nowMs)).toBe("Not deployed");
	});

	it("supports a caller-provided absent label", () => {
		expect(formatRelativeTime(undefined, nowMs, "Never")).toBe("Never");
	});

	it("defends against future clock skew", () => {
		expect(formatRelativeTime(new Date(nowMs + 5_000), nowMs)).toBe("just now");
		expect(formatRelativeTime(new Date(nowMs + 3_600_000), nowMs)).toBe(
			"just now",
		);
	});

	it("formats valid past times relatively", () => {
		expect(formatRelativeTime(new Date(nowMs - 5_000), nowMs)).toBe(
			"5 seconds ago",
		);
		expect(formatRelativeTime(new Date(nowMs - 5 * 60_000), nowMs)).toBe(
			"5 minutes ago",
		);
		expect(formatRelativeTime(new Date(nowMs - 3 * 3_600_000), nowMs)).toBe(
			"3 hours ago",
		);
		expect(formatRelativeTime(new Date(nowMs - 2 * 86_400_000), nowMs)).toBe(
			"2 days ago",
		);
	});
});

describe("formatTimeUntil", () => {
	const now = Date.UTC(2026, 0, 1);

	it("counts down to a future moment in the coarsest useful unit", () => {
		expect(formatTimeUntil(new Date(now + 6 * 86_400_000), now)).toBe(
			"in 6 days",
		);
		expect(formatTimeUntil(new Date(now + 5 * 3_600_000), now)).toBe(
			"in 5 hours",
		);
		expect(formatTimeUntil(new Date(now + 12 * 60_000), now)).toBe(
			"in 12 minutes",
		);
	});

	it("reports a due moment instead of a negative distance", () => {
		expect(formatTimeUntil(new Date(now - 1_000), now)).toBe("any moment now");
	});
});
