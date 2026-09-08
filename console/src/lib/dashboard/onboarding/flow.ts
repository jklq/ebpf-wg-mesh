import type { DashboardOnboardingDraft } from "#/lib/dashboard/core/types.server";

export function defaultOnboardingDraft(): DashboardOnboardingDraft {
	return {
		projectId: "",
		environmentId: "",
		serviceId: "",
		repositorySelector: "",
		trackedRef: "",
		dockerfilePath: "",
		contextDir: "",
		hostname: "",
	};
}

export function normalizeRepositorySelector(value: string): string {
	const [owner, repo, ...rest] = value
		.trim()
		.split("/")
		.map((part) => part.trim().toLowerCase())
		.filter((part) => part !== "");
	if (!owner || !repo || rest.length > 0) {
		throw new Error("Repository must be in owner/repo form.");
	}
	return `${owner}/${repo}`;
}

export function slugifyServiceName(value: string): string {
	const slug = value
		.trim()
		.toLowerCase()
		.replace(/[^a-z0-9]+/g, "-")
		.replace(/^-+|-+$/g, "");
	return slug || "service";
}

const generatedServiceAdjectives = [
	"brisk",
	"calm",
	"clever",
	"lively",
	"steady",
	"talented",
	"vivid",
	"warm",
];

const generatedServiceNouns = [
	"harbor",
	"harmony",
	"lantern",
	"meadow",
	"signal",
	"summit",
	"tempo",
	"workshop",
];

export function generatedServiceNameFromSeed(seed: string): string {
	let hash = 0;
	for (let index = 0; index < seed.length; index += 1) {
		hash = (hash * 31 + seed.charCodeAt(index)) >>> 0;
	}
	const adjective =
		generatedServiceAdjectives[hash % generatedServiceAdjectives.length] ??
		"steady";
	const noun =
		generatedServiceNouns[
			Math.floor(hash / generatedServiceAdjectives.length) %
				generatedServiceNouns.length
		] ?? "service";
	return `${adjective}-${noun}`;
}

export function uniqueServiceName(
	existingNames: Iterable<string>,
	preferredName: string,
): string {
	const base = slugifyServiceName(preferredName);
	const names = new Set(Array.from(existingNames));
	if (!names.has(base)) {
		return base;
	}
	for (let index = 2; index < 1000; index += 1) {
		const candidate = `${base}-${index}`;
		if (!names.has(candidate)) {
			return candidate;
		}
	}
	return `${base}-${Date.now()}`;
}
