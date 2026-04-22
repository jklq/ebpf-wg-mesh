import type {
	DashboardBuildRecipe,
	DashboardOnboardingDraft,
	DashboardOnboardingStep,
	DashboardRepositoryInspection,
} from "#/lib/dashboard/core/types.server";

export function defaultOnboardingDraft(): DashboardOnboardingDraft {
	return {
		currentStep: "account",
		projectId: "",
		serviceId: "",
		repositorySelector: "",
		trackedRef: "",
		dockerfilePath: "",
		contextDir: "",
		containerPort: "",
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

export function repositoryBasename(selector: string): string {
	return normalizeRepositorySelector(selector).split("/")[1] ?? "";
}

export function slugifyServiceName(value: string): string {
	const slug = value
		.trim()
		.toLowerCase()
		.replace(/[^a-z0-9]+/g, "-")
		.replace(/^-+|-+$/g, "")
		.replace(/-{2,}/g, "-");
	return slug || "service";
}

export function applyInspectionDefaults(
	draft: DashboardOnboardingDraft,
	inspection: DashboardRepositoryInspection,
): DashboardOnboardingDraft {
	const recommended = inspection.recommendedBuildRecipe;
	return {
		...draft,
		currentStep: nextOnboardingStep(
			inspection.accessState === "available" ? "build" : "repository",
		),
		trackedRef: draft.trackedRef || inspection.defaultBranch,
		dockerfilePath: draft.dockerfilePath || recommended?.dockerfilePath || "",
		contextDir: draft.contextDir || recommended?.contextDir || "",
	};
}

export function recommendedBuildRecipeFromCandidates(
	candidates: Array<string>,
): DashboardBuildRecipe | undefined {
	if (candidates.length === 0) {
		return undefined;
	}
	const sorted = [...candidates].sort((left, right) =>
		left.localeCompare(right),
	);
	const dockerfilePath = sorted.includes("Dockerfile")
		? "Dockerfile"
		: (sorted[0] ?? "");
	if (!dockerfilePath) {
		return undefined;
	}
	const slashIndex = dockerfilePath.lastIndexOf("/");
	return {
		dockerfilePath,
		contextDir: slashIndex === -1 ? "." : dockerfilePath.slice(0, slashIndex),
	};
}

export function nextOnboardingStep(
	step: DashboardOnboardingStep,
): DashboardOnboardingStep {
	return step;
}
