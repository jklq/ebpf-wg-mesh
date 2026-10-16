import { formatBytes } from "#/lib/bytes";
import type {
	DashboardProjectUsage,
	DashboardUsageBucket,
	DashboardUsageQuantities,
	DashboardUsageResolution,
} from "#/lib/dashboard/core/types.server";
import { dateMillis } from "#/lib/time";

const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

export const usageRanges = {
	"24h": {
		label: "24 h",
		spanMs: DAY_MS,
		stepMs: MINUTE_MS,
		resolution: "USAGE_RESOLUTION_MINUTE",
	},
	"7d": {
		label: "7 d",
		spanMs: 7 * DAY_MS,
		stepMs: HOUR_MS,
		resolution: "USAGE_RESOLUTION_HOUR",
	},
	"30d": {
		label: "30 d",
		spanMs: 30 * DAY_MS,
		stepMs: HOUR_MS,
		resolution: "USAGE_RESOLUTION_HOUR",
	},
	"90d": {
		label: "90 d",
		spanMs: 90 * DAY_MS,
		stepMs: DAY_MS,
		resolution: "USAGE_RESOLUTION_DAY",
	},
} as const satisfies Record<
	string,
	{
		label: string;
		spanMs: number;
		stepMs: number;
		resolution: DashboardUsageResolution;
	}
>;
export type UsageRange = keyof typeof usageRanges;

/**
 * The query window for a range ending at the current bucket. The server widens
 * ranges to whole buckets before checking the resolution limit, so the end is
 * aligned here and the start derived from it.
 */
export function usageWindow(range: UsageRange, nowMs: number) {
	const { spanMs, stepMs, resolution } = usageRanges[range];
	const end = Math.ceil(nowMs / stepMs) * stepMs;
	return {
		startTime: new Date(end - spanMs).toISOString(),
		endTime: new Date(end).toISOString(),
		resolution,
	};
}

type UsageDouble = DashboardUsageQuantities["averageVcpu"];

/** Protobuf JSON encodes non-finite doubles as strings; those are not values. */
export function finiteDouble(
	value: UsageDouble | undefined,
): number | undefined {
	return typeof value === "number" && Number.isFinite(value)
		? value
		: undefined;
}

/**
 * Missing buckets have no value to draw: past retention, inside a gap, or not
 * yet measured. A complete bucket without intervals is a measured zero.
 */
export type BucketState =
	| "complete"
	| "incomplete"
	| "pending"
	| "gap"
	| "expired";

export function bucketState(
	bucket: DashboardUsageBucket,
	usage: Pick<DashboardProjectUsage, "retainedSince">,
): BucketState {
	const retainedSince = dateMillis(usage.retainedSince);
	if (retainedSince && dateMillis(bucket.end) <= retainedSince)
		return "expired";
	const measured = (bucket.usage?.measuredMilliseconds ?? "0") !== "0";
	const gap = (bucket.usage?.gapMilliseconds ?? "0") !== "0";
	if (!measured && gap) return "gap";
	if (!bucket.complete) return measured ? "incomplete" : "pending";
	return "complete";
}

export function isMissing(state: BucketState): boolean {
	return state === "pending" || state === "gap" || state === "expired";
}

/** Usage was reported for the range, so zero quantities are real zeros. */
export function hasUsageData(usage: DashboardProjectUsage): boolean {
	return usage.watermark !== undefined || usage.resources.length > 0;
}

function formatDecimal(value: number): string {
	if (value === 0) return "0";
	if (Math.abs(value) < 0.001) return "<0.001";
	return new Intl.NumberFormat("en", { maximumSignificantDigits: 3 }).format(
		value,
	);
}

export function formatVcpu(value: number): string {
	return `${formatDecimal(value)} vCPU`;
}

/** Display-only: the exact integer string can exceed a double's precision. */
export function formatCpuTime(microseconds: string): string {
	const seconds = Number(microseconds) / 1_000_000;
	if (seconds < 60) return `${formatDecimal(seconds)} CPU·s`;
	if (seconds < 3600) return `${formatDecimal(seconds / 60)} CPU·min`;
	return `${formatDecimal(seconds / 3600)} CPU·h`;
}

export function formatAverageBytes(bytes: number): string {
	return formatBytes(Math.round(bytes));
}

/** A byte-millisecond integral in byte-hours, e.g. "1.5 GiB·h". */
export function formatByteHours(byteMilliseconds: string): string {
	const byteHours = Number(byteMilliseconds) / HOUR_MS;
	return `${formatBytes(byteHours)}·h`;
}

export function formatByteCount(bytes: string): string {
	return formatBytes(Number(bytes));
}

export const usageMetrics = {
	cpu: {
		label: "CPU",
		value: (usage: DashboardUsageQuantities) => finiteDouble(usage.averageVcpu),
		format: formatVcpu,
	},
	memory: {
		label: "Memory",
		value: (usage: DashboardUsageQuantities) =>
			finiteDouble(usage.averageMemoryBytes),
		format: formatAverageBytes,
	},
	volume: {
		label: "Volumes",
		value: (usage: DashboardUsageQuantities) =>
			finiteDouble(usage.averageVolumeBytes),
		format: formatAverageBytes,
	},
	egress: {
		label: "Public output",
		value: (usage: DashboardUsageQuantities) => Number(usage.publicEgressBytes),
		format: (bytes: number) => formatBytes(bytes),
	},
} as const;
export type UsageMetric = keyof typeof usageMetrics;

const csvQuantities = [
	"cpuMicroseconds",
	"memoryByteMilliseconds",
	"volumeByteMilliseconds",
	"retainedVolumeByteMilliseconds",
	"publicEgressBytes",
	"publicDirectBytes",
	"publicIngressBytes",
	"privateBytes",
	"platformBytes",
	"probeBytes",
	"measuredMilliseconds",
	"gapMilliseconds",
	"memoryCoveredMilliseconds",
	"volumeCoveredMilliseconds",
	"uncertainMilliseconds",
] as const satisfies readonly (keyof DashboardUsageQuantities)[];

const csvHeader = [
	"scope",
	"resource_id",
	"name",
	"environment",
	"service_id",
	"deleted_at",
	"bucket_start",
	"bucket_end",
	"complete",
	...csvQuantities.map((key) =>
		key.replace(/[A-Z]/g, (letter) => `_${letter.toLowerCase()}`),
	),
	"estimated",
];

function csvField(value: string): string {
	return /[",\n\r]/.test(value) ? `"${value.replaceAll('"', '""')}"` : value;
}

/**
 * One row per bucket for the project and for each resource. Quantities are the
 * exact decimal integers from the platform; averages are left out.
 */
export function usageCsv(usage: DashboardProjectUsage): string {
	const row = (identity: string[], bucket: DashboardUsageBucket): string[] => [
		...identity,
		bucket.start ?? "",
		bucket.end ?? "",
		String(bucket.complete),
		...csvQuantities.map((key) => bucket.usage?.[key] ?? "0"),
		String(bucket.usage?.estimated ?? false),
	];
	const rows = [
		csvHeader,
		...usage.buckets.map((bucket) =>
			row(["project", usage.projectId, "", "", "", ""], bucket),
		),
		...usage.resources.flatMap((resource) =>
			resource.buckets.map((bucket) =>
				row(
					[
						resource.kind,
						resource.resourceId,
						resource.name,
						resource.environmentName,
						resource.serviceId,
						resource.deletedAt ?? "",
					],
					bucket,
				),
			),
		),
	];
	return `${rows.map((fields) => fields.map(csvField).join(",")).join("\n")}\n`;
}
