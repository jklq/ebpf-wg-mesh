export function readPositiveResource(
	value: Record<string, unknown> | undefined,
	key: string,
	fallback: number,
): number {
	const resource = readOptionalNumber(value, key);
	return resource !== undefined && resource > 0 ? resource : fallback;
}

export function readRecord(
	raw: unknown,
	context: string,
): Record<string, unknown> {
	if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
		throw new Error(`invalid ${context}: expected an object`);
	}
	return raw as Record<string, unknown>;
}

export function readOptionalRecord(
	raw: unknown,
): Record<string, unknown> | undefined {
	if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
		return undefined;
	}
	return raw as Record<string, unknown>;
}

export function readArray(
	value: Record<string, unknown>,
	key: string,
): Array<unknown> {
	const array = value[key];
	if (!Array.isArray(array)) {
		return [];
	}
	return array;
}

export function readRequiredString(
	value: Record<string, unknown>,
	key: string,
	context: string,
): string {
	const candidate = value[key];
	if (typeof candidate !== "string" || candidate === "") {
		throw new Error(`invalid ${context}.${key}`);
	}
	return candidate;
}

export function readOptionalString(
	value: Record<string, unknown> | undefined,
	key: string,
): string | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	return typeof candidate === "string" ? candidate : undefined;
}

export function readOptionalNumber(
	value: Record<string, unknown> | undefined,
	key: string,
): number | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	return typeof candidate === "number" ? candidate : undefined;
}

export function readOptionalNumberLike(
	value: Record<string, unknown> | undefined,
	key: string,
): number | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	if (typeof candidate === "number") {
		return Number.isFinite(candidate) ? candidate : undefined;
	}
	if (typeof candidate === "string" && candidate.trim() !== "") {
		const parsed = Number(candidate);
		return Number.isFinite(parsed) ? parsed : undefined;
	}
	return undefined;
}

export function readRequiredNumber(
	value: Record<string, unknown>,
	key: string,
	context: string,
): number {
	const candidate = readOptionalNumber(value, key);
	if (candidate === undefined) {
		throw new Error(`invalid ${context}.${key}`);
	}
	return candidate;
}

export function readBoolean(
	value: Record<string, unknown>,
	key: string,
): boolean {
	return value[key] === true;
}

export function readOptionalDate(
	value: Record<string, unknown> | undefined,
	key: string,
): Date | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	if (candidate instanceof Date) {
		return candidate;
	}
	if (typeof candidate === "string" || typeof candidate === "number") {
		const parsed = new Date(candidate);
		return Number.isNaN(parsed.getTime()) ? undefined : parsed;
	}
	if (candidate && typeof candidate === "object") {
		const record = candidate as Record<string, unknown>;
		const seconds = readOptionalNumberLike(record, "seconds");
		const nanos = readOptionalNumberLike(record, "nanos") ?? 0;
		if (seconds !== undefined) {
			return new Date(seconds * 1000 + nanos / 1_000_000);
		}
		const toDate = (candidate as { toDate?: () => Date }).toDate;
		if (typeof toDate === "function") {
			return toDate.call(candidate);
		}
	}
	return undefined;
}

export function readStringArray(
	value: Record<string, unknown>,
	key: string,
): Array<string> {
	return readArray(value, key).filter(
		(entry): entry is string => typeof entry === "string",
	);
}

export function readStringMap(
	value: Record<string, unknown> | undefined,
	key: string,
): Record<string, string> {
	const record = readOptionalRecord(value?.[key]);
	if (!record) {
		return {};
	}
	const out: Record<string, string> = {};
	for (const [entryKey, entryValue] of Object.entries(record)) {
		if (typeof entryValue === "string") {
			out[entryKey] = entryValue;
		}
	}
	return out;
}

export function readNumberArray(
	value: Record<string, unknown>,
	key: string,
): Array<number> {
	return readArray(value, key).filter(
		(entry): entry is number =>
			typeof entry === "number" && Number.isFinite(entry),
	);
}

export function readNumberLikeValue(raw: unknown, context: string): number {
	const value =
		typeof raw === "number"
			? raw
			: typeof raw === "string" && raw.trim() !== ""
				? Number(raw)
				: Number.NaN;
	if (!Number.isFinite(value)) {
		throw new Error(`invalid ${context}`);
	}
	return value;
}
