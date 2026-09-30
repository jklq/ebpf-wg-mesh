export const NOT_DEPLOYED_LABEL = "Not deployed";

export function cleanDate(value: unknown): Date | undefined {
	if (!value) {
		return undefined;
	}
	if (value instanceof Date) {
		const ms = value.getTime();
		if (!Number.isFinite(ms) || ms === 0 || ms === -62135596800000) {
			return undefined;
		}
		return value;
	}
	if (typeof value === "string" || typeof value === "number") {
		if (
			typeof value === "string" &&
			/^0001-01-01T00:00:00(?:\.0+)?Z$/.test(value)
		)
			return undefined;
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
	value: Date | string | undefined | null,
	nowMs: number = Date.now(),
	absentLabel: string = NOT_DEPLOYED_LABEL,
): string {
	const date = cleanDate(value);
	if (!date) {
		return absentLabel;
	}
	const diffMs = date.getTime() - nowMs;
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

export function formatLogTime(value: Date | string): string {
	const date = cleanDate(value);
	if (!date) return "—";
	return new Intl.DateTimeFormat(undefined, {
		hour: "2-digit",
		minute: "2-digit",
		second: "2-digit",
		hour12: false,
	}).format(date);
}

/** "in 6 days" style distance to a future moment; "any moment now" once due. */
export function formatTimeUntil(
	value: Date | string,
	nowMs: number = Date.now(),
): string {
	const date = cleanDate(value);
	const diffMs = date ? date.getTime() - nowMs : 0;
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

export function formatDateTime(value: Date | string): string {
	const date = cleanDate(value);
	if (!date) return "—";
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

export function dateMillis(value: Date | string | undefined | null): number {
	return cleanDate(value)?.getTime() ?? 0;
}
