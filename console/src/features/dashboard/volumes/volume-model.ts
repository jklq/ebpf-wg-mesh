import type { HealthTone } from "#/components/ui/status-dot";
import type {
	DashboardServiceRecord,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { safeInteger } from "#/lib/platform-json";

export const DEFAULT_VOLUME_MOUNT_PATH = "/data";
export const DEFAULT_VOLUME_SIZE_GIB = 5;
export const MIN_VOLUME_SIZE_GIB = 1;
export const MAX_VOLUME_SIZE_GIB = 4096;

/** The service whose draft mounts the volume, if any. */
export function volumeOwner(
	volume: DashboardVolume,
	services: Array<DashboardServiceRecord>,
): DashboardServiceRecord | undefined {
	return services.find(
		(service) => service.spec?.runtime?.volume?.volumeName === volume.name,
	);
}

/** The volume a service's draft mounts, if it exists in the environment. */
export function serviceVolume(
	service: DashboardServiceRecord,
	volumes: Array<DashboardVolume>,
): DashboardVolume | undefined {
	const name = service.spec?.runtime?.volume?.volumeName;
	return name ? volumes.find((volume) => volume.name === name) : undefined;
}

export function unattachedVolumes(
	volumes: Array<DashboardVolume>,
	services: Array<DashboardServiceRecord>,
): Array<DashboardVolume> {
	return volumes.filter((volume) => !volumeOwner(volume, services));
}

export function volumeUsage(volume: DashboardVolume): {
	used: number;
	size: number;
	ratio: number;
} {
	const used = safeInteger(volume.usedBytes || "0");
	const size = safeInteger(volume.sizeBytes || "0");
	return { used, size, ratio: size > 0 ? Math.min(used / size, 1) : 0 };
}

export function volumeTone(volume: DashboardVolume): HealthTone {
	switch (volume.state) {
		case "VOLUME_STATE_READY":
			return "healthy";
		case "VOLUME_STATE_FULL":
		case "VOLUME_STATE_ERROR":
		case "VOLUME_STATE_UNAVAILABLE":
			return "failed";
		case "VOLUME_STATE_PENDING":
			return "building";
		default:
			return "offline";
	}
}

export function volumeStateLabel(volume: DashboardVolume): string {
	switch (volume.state) {
		case "VOLUME_STATE_READY":
			return "Ready";
		case "VOLUME_STATE_FULL":
			return "Full";
		case "VOLUME_STATE_UNAVAILABLE":
			return "Unavailable";
		case "VOLUME_STATE_ERROR":
			return "Error";
		case "VOLUME_STATE_PENDING":
			return "Pending";
		default:
			return "Unknown";
	}
}

const adjectives = [
	"amber",
	"brisk",
	"calm",
	"dapper",
	"eager",
	"fuzzy",
	"gentle",
	"hardy",
	"jolly",
	"keen",
	"lucky",
	"mellow",
	"nimble",
	"plucky",
	"quiet",
	"rustic",
	"sturdy",
	"tidy",
	"vivid",
	"witty",
];
const nouns = [
	"acorn",
	"badger",
	"cellar",
	"dune",
	"ember",
	"fjord",
	"grove",
	"harbor",
	"igloo",
	"juniper",
	"kettle",
	"lantern",
	"meadow",
	"nectar",
	"orchard",
	"pantry",
	"quarry",
	"reservoir",
	"silo",
	"vault",
];

/** A readable generated name for a new volume. */
export function suggestVolumeName(
	taken: Array<string>,
	random: () => number = Math.random,
): string {
	const pick = (words: Array<string>) =>
		words[Math.floor(random() * words.length)] ?? words[0];
	for (let attempt = 0; attempt < 20; attempt += 1) {
		const name = `${pick(adjectives)}-${pick(nouns)}`;
		if (!taken.includes(name)) return name;
	}
	return `volume-${Date.now().toString(36)}`;
}

const SYSTEM_MOUNT_ROOTS = [
	"/bin",
	"/boot",
	"/dev",
	"/etc",
	"/lib",
	"/lib32",
	"/lib64",
	"/libx32",
	"/proc",
	"/run",
	"/sbin",
	"/sys",
	"/usr",
	"/var/run",
];

/** Mirrors the control plane's check so mistakes surface while typing. */
export function mountPathError(path: string): string | undefined {
	const value = path.trim();
	if (!value) return "Mount path is required";
	if (!value.startsWith("/")) return "Mount path must be absolute";
	if (value === "/") return "Cannot mount over the container root";
	if (/[\s,:\\]/.test(value))
		return "Mount path contains an unsupported character";
	if (
		value.endsWith("/") ||
		/(^|\/)\.\.?(\/|$)/.test(value) ||
		value.includes("//")
	)
		return "Use a clean path without trailing slashes or dot segments";
	const root = SYSTEM_MOUNT_ROOTS.find(
		(entry) => value === entry || value.startsWith(`${entry}/`),
	);
	if (root) return `Cannot mount inside system directory ${root}`;
	return undefined;
}

export function sizeGibError(raw: string, minimum = MIN_VOLUME_SIZE_GIB) {
	const value = Number(raw.trim());
	if (!Number.isFinite(value) || raw.trim() === "")
		return "Enter a size in GiB";
	if (value < minimum) return `Size must be at least ${minimum} GiB`;
	if (value > MAX_VOLUME_SIZE_GIB)
		return `Size must be at most ${MAX_VOLUME_SIZE_GIB} GiB`;
	if (!Number.isInteger(value * 1024)) return "Use at most three decimals";
	return undefined;
}
