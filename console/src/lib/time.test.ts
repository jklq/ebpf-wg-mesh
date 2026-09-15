import { create } from "@bufbuild/protobuf";
import { TimestampSchema, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
	cleanDate,
	formatRelativeTime,
	protoTimestampToDate,
} from "#/lib/time";

describe("protoTimestampToDate", () => {
	it("keeps missing timestamps absent", () => {
		expect(protoTimestampToDate(undefined)).toBeUndefined();
		expect(protoTimestampToDate(null)).toBeUndefined();
	});

	it("keeps zero and epoch values absent", () => {
		expect(
			protoTimestampToDate(create(TimestampSchema, { seconds: 0n, nanos: 0 })),
		).toBeUndefined();
		expect(
			protoTimestampToDate(
				create(TimestampSchema, { seconds: -62135596800n, nanos: 0 }),
			),
		).toBeUndefined();
	});

	it("keeps malformed timestamps absent", () => {
		expect(
			protoTimestampToDate(
				create(TimestampSchema, { seconds: 100n, nanos: 2_000_000_000 }),
			),
		).toBeUndefined();
		expect(
			protoTimestampToDate(
				create(TimestampSchema, {
					seconds: 99999999999999999999999n,
					nanos: 0,
				}),
			),
		).toBeUndefined();
	});

	it("passes future timestamps through for the formatter to defend", () => {
		const future = new Date(Date.now() + 3_600_000);
		expect(protoTimestampToDate(timestampFromDate(future))).toEqual(future);
	});

	it("converts valid timestamps", () => {
		const moment = new Date("2026-08-13T10:00:22.000Z");
		expect(protoTimestampToDate(timestampFromDate(moment))).toEqual(moment);
	});
});

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
