import { type Timestamp, timestampDate } from "@bufbuild/protobuf/wkt";

export const NOT_DEPLOYED_LABEL = "Not deployed";

// google.protobuf.Timestamp range: 0001-01-01T00:00:00Z to 9999-12-31T23:59:59.999999999Z.
// Go's zero time serializes to the range minimum, so it stays absent.
const MIN_TIMESTAMP_SECONDS = -62135596800n;
const MAX_TIMESTAMP_SECONDS = 253402300799n;
const MAX_NANOS = 999_999_999;

export function protoTimestampToDate(
	value: Timestamp | undefined | null,
): Date | undefined {
	if (!value) {
		return undefined;
	}
	try {
		const seconds =
			typeof value.seconds === "bigint"
				? value.seconds
				: BigInt(value.seconds ?? 0);
		const nanos = value.nanos ?? 0;
		if (seconds === 0n && nanos === 0) {
			return undefined;
		}
		if (seconds === MIN_TIMESTAMP_SECONDS && nanos === 0) {
			return undefined;
		}
		if (seconds < MIN_TIMESTAMP_SECONDS || seconds > MAX_TIMESTAMP_SECONDS) {
			return undefined;
		}
		if (!Number.isInteger(nanos) || nanos < 0 || nanos > MAX_NANOS) {
			return undefined;
		}
		const date = timestampDate(value);
		const ms = date.getTime();
		if (!Number.isFinite(ms) || ms === 0) {
			return undefined;
		}
		return date;
	} catch {
		return undefined;
	}
}

export function cleanDate(value: unknown): Date | undefined {
	if (!value) {
		return undefined;
	}
	if (value instanceof Date) {
		const ms = value.getTime();
		if (!Number.isFinite(ms) || ms === 0) {
			return undefined;
		}
		return value;
	}
	if (typeof value === "string" || typeof value === "number") {
		const date = new Date(value);
		const ms = date.getTime();
		if (!Number.isFinite(ms) || ms === 0) {
			return undefined;
		}
		return date;
	}
	return undefined;
}

export function formatRelativeTime(
	value: Date | undefined | null,
	nowMs: number = Date.now(),
	absentLabel: string = NOT_DEPLOYED_LABEL,
): string {
	if (!value || !Number.isFinite(value.getTime()) || value.getTime() === 0) {
		return absentLabel;
	}
	const diffMs = value.getTime() - nowMs;
	if (diffMs > 0) {
		return "just now";
	}
	const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
	const absSeconds = Math.round(Math.abs(diffMs) / 1000);
	if (absSeconds < 60) {
		return rtf.format(Math.round(diffMs / 1000), "second");
	}
	const absMinutes = Math.round(absSeconds / 60);
	if (absMinutes < 60) {
		return rtf.format(Math.round(diffMs / 60_000), "minute");
	}
	const absHours = Math.round(absMinutes / 60);
	if (absHours < 24) {
		return rtf.format(Math.round(diffMs / 3_600_000), "hour");
	}
	return rtf.format(Math.round(diffMs / 86_400_000), "day");
}

export function formatDuration(ms: number): string {
	const seconds = Math.max(0, Math.round(ms / 1000));
	if (seconds < 60) {
		return `${seconds}s`;
	}
	return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

export function formatLogTime(date: Date): string {
	return new Intl.DateTimeFormat(undefined, {
		hour: "2-digit",
		minute: "2-digit",
		second: "2-digit",
		hour12: false,
	}).format(date);
}

/** "in 6 days" style distance to a future moment; "any moment now" once due. */
export function formatTimeUntil(
	value: Date,
	nowMs: number = Date.now(),
): string {
	const diffMs = value.getTime() - nowMs;
	if (!Number.isFinite(diffMs) || diffMs <= 60_000) {
		return "any moment now";
	}
	const rtf = new Intl.RelativeTimeFormat("en", { numeric: "always" });
	const minutes = Math.round(diffMs / 60_000);
	if (minutes < 60) {
		return rtf.format(minutes, "minute");
	}
	const hours = Math.round(minutes / 60);
	if (hours < 48) {
		return rtf.format(hours, "hour");
	}
	return rtf.format(Math.round(hours / 24), "day");
}

export function formatDateTime(date: Date): string {
	return new Intl.DateTimeFormat("en", {
		timeZone: "UTC",
		timeZoneName: "short",
		year: "numeric",
		month: "short",
		day: "numeric",
		hour: "2-digit",
		minute: "2-digit",
		hour12: false,
	}).format(date);
}
