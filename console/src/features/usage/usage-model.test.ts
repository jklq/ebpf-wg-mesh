import { describe, expect, it } from "vitest";
import {
	axisMax,
	axisTicks,
	bucketState,
	finiteDouble,
	formatCpuTime,
	usageCsv,
	usageWindow,
} from "#/features/usage/usage-model";
import type {
	DashboardProjectUsage,
	DashboardUsageBucket,
	DashboardUsageQuantities,
} from "#/lib/dashboard/core/types.server";

const quantities = (
	overrides: Partial<DashboardUsageQuantities> = {},
): DashboardUsageQuantities => ({
	cpuMicroseconds: "0",
	memoryByteMilliseconds: "0",
	volumeByteMilliseconds: "0",
	retainedVolumeByteMilliseconds: "0",
	publicEgressBytes: "0",
	publicDirectBytes: "0",
	publicIngressBytes: "0",
	privateBytes: "0",
	platformBytes: "0",
	probeBytes: "0",
	measuredMilliseconds: "0",
	gapMilliseconds: "0",
	memoryCoveredMilliseconds: "0",
	volumeCoveredMilliseconds: "0",
	uncertainMilliseconds: "0",
	windowMilliseconds: "60000",
	averageVcpu: 0,
	averageMemoryBytes: 0,
	averageVolumeBytes: 0,
	estimated: false,
	...overrides,
});

const bucket = (
	start: string,
	complete: boolean,
	usage: Partial<DashboardUsageQuantities> = {},
): DashboardUsageBucket => ({
	start,
	end: new Date(Date.parse(start) + 60_000).toISOString(),
	complete,
	usage: quantities(usage),
});

describe("usageWindow", () => {
	it("aligns the end up to the bucket so the server does not widen past the limit", () => {
		const now = Date.parse("2026-10-16T12:00:30.000Z");
		expect(usageWindow("24h", now)).toEqual({
			startTime: "2026-10-15T12:01:00.000Z",
			endTime: "2026-10-16T12:01:00.000Z",
			resolution: "USAGE_RESOLUTION_MINUTE",
		});
		expect(usageWindow("90d", now)).toMatchObject({
			startTime: "2026-07-19T00:00:00.000Z",
			endTime: "2026-10-17T00:00:00.000Z",
			resolution: "USAGE_RESOLUTION_DAY",
		});
	});
});

describe("finiteDouble", () => {
	it("treats protobuf JSON non-finite strings as absent", () => {
		expect(finiteDouble(0.25)).toBe(0.25);
		expect(finiteDouble("NaN")).toBeUndefined();
		expect(finiteDouble("Infinity")).toBeUndefined();
		expect(finiteDouble(undefined)).toBeUndefined();
	});
});

describe("bucketState", () => {
	const usage = { retainedSince: "2026-10-01T00:00:00Z" };

	it("tells missing buckets apart from measured zeros", () => {
		expect(bucketState(bucket("2026-10-16T10:00:00Z", true), usage)).toBe(
			"complete",
		);
		expect(bucketState(bucket("2026-10-16T10:00:00Z", false), usage)).toBe(
			"pending",
		);
		expect(
			bucketState(
				bucket("2026-10-16T10:00:00Z", true, { gapMilliseconds: "60000" }),
				usage,
			),
		).toBe("gap");
		expect(bucketState(bucket("2026-09-01T00:00:00Z", true), usage)).toBe(
			"expired",
		);
	});

	it("keeps measured values of incomplete buckets", () => {
		expect(
			bucketState(
				bucket("2026-10-16T10:00:00Z", false, { measuredMilliseconds: "5000" }),
				usage,
			),
		).toBe("incomplete");
	});
});

describe("axisMax", () => {
	it("uses the limit while usage stays within it", () => {
		expect(axisMax(0.1, 0.25, "vcpu")).toBe(0.25);
		expect(axisMax(0, 512 * 1024 ** 2, "bytes")).toBe(512 * 1024 ** 2);
	});

	it("rounds above data with no or an exceeded limit", () => {
		expect(axisMax(3, undefined, "vcpu")).toBe(4);
		expect(axisMax(0.3, 0.25, "vcpu")).toBe(0.4);
		expect(axisMax(300 * 1024, undefined, "bytes")).toBe(400 * 1024);
	});
});

describe("axisTicks", () => {
	it("steps in round numbers and ends on the axis top", () => {
		expect(axisTicks(1, "vcpu")).toEqual([0, 0.25, 0.5, 0.75, 1]);
		expect(axisTicks(0.25, "vcpu")).toEqual([0, 0.1, 0.2, 0.25]);
		const mib = 1024 ** 2;
		expect(axisTicks(256 * mib, "bytes")).toEqual([
			0,
			100 * mib,
			200 * mib,
			256 * mib,
		]);
	});
});

describe("formatCpuTime", () => {
	it("picks a unit for the duration", () => {
		expect(formatCpuTime("1500000")).toBe("1.5 CPU·s");
		expect(formatCpuTime("7200000000")).toBe("2 CPU·h");
	});
});

describe("usageCsv", () => {
	it("exports exact integers per bucket for the project and each resource", () => {
		const exact = "123456789012345678901";
		const start = "2026-10-16T10:00:00Z";
		const usage: DashboardProjectUsage = {
			projectId: "project-1",
			resolution: "USAGE_RESOLUTION_MINUTE",
			meterVersion: "usage/v1",
			buckets: [bucket(start, true, { memoryByteMilliseconds: exact })],
			resources: [
				{
					kind: "allocation",
					resourceId: "alloc-1",
					environmentId: "env-1",
					environmentName: "production",
					serviceId: "service-1",
					name: 'web, "api"',
					deploymentId: "deploy-1",
					buckets: [bucket(start, true, { memoryByteMilliseconds: exact })],
				},
			],
			gaps: [],
			gapsTruncated: false,
			stale: false,
			staleReason: "",
		};
		const [header, project, resource] = usageCsv(usage).trimEnd().split("\n");
		const column = header.split(",").indexOf("memory_byte_milliseconds");
		expect(project.split(",")[column]).toBe(exact);
		expect(resource).toContain('"web, ""api"""');
		expect(resource).toContain(exact);
	});
});
