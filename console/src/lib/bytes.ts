import { safeInteger } from "#/lib/platform-json";

const GIB = 1024 ** 3;
const MIB = 1024 ** 2;

/** Formats a protobuf int64 byte count with binary units. */
export function formatBytes(raw: string | number): string {
	const bytes = typeof raw === "number" ? raw : safeInteger(raw);
	const units = ["B", "KiB", "MiB", "GiB", "TiB"];
	let value = bytes;
	let unit = 0;
	while (value >= 1024 && unit < units.length - 1) {
		value /= 1024;
		unit += 1;
	}
	return `${Number.isInteger(value) ? value : value.toFixed(1)} ${units[unit]}`;
}

export function gibToBytes(gib: number): number {
	return Math.round(gib * 1024) * MIB;
}

export function bytesToGib(raw: string | number): number {
	const bytes = typeof raw === "number" ? raw : safeInteger(raw);
	return bytes / GIB;
}
