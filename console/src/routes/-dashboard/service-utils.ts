import type {
	DashboardDeploymentStage,
	DashboardDeploymentStageState,
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import type { ServiceHealth } from "./types";

export function serviceHealth(service: DashboardServiceRecord): ServiceHealth {
	const deployment = service.latestDeployment;
	if (deployment) {
		switch (deployment.state) {
			case "active":
			case "draining":
			case "completed":
				return "healthy";
			case "failed":
			case "crashed":
			case "cancelled":
				return "failed";
			case "removed":
			case "superseded":
				return "offline";
			default:
				return "building";
		}
	}
	const build = service.latestBuild;
	if (!build) return "offline";
	const stageState = deploymentStageHealth(build.stages ?? []);
	if (stageState === "failed") return "failed";
	if (stageState === "active") return "building";
	if (build.state === "failed") return "failed";
	if (build.state === "running" || build.state === "queued") return "building";
	if (build.state === "succeeded") return "healthy";
	return "offline";
}

export function healthLabel(h: ServiceHealth): string {
	switch (h) {
		case "healthy":
			return "Healthy";
		case "building":
			return "Building";
		case "failed":
			return "Failed";
		case "offline":
			return "Offline";
	}
}

function toActiveLabel(label: string): string {
	if (label === "Deploy") return "Deploying";
	if (label === "Build") return "Building";
	if (label === "Post-deploy") return "Checking health";
	return label.endsWith("ing") ? label : `${label}ing`;
}

/**
 * More granular status label for the hero — tracks stage-by-stage progress so
 * the text changes whenever the stage bar changes:
 *   queued → (first stage label) → … → (last stage label) → Healthy / Failed
 */
export function heroStatusLabel(
	build: DashboardServiceRecord["latestBuild"],
): string {
	if (!build) return "Offline";
	const failedStage = build.stages?.find((s) => s.state === "failed");
	if (failedStage) return "Failed";
	if (build.state === "failed") return "Failed";

	// A running stage always wins — it gives the most precise label (e.g.
	// "Deploying") and must be checked before the build-level state so we don't
	// jump to "Healthy" while a deploy stage is still in progress.
	const runningStage = build.stages?.find((s) => s.state === "running");
	if (runningStage?.label) return toActiveLabel(runningStage.label);

	if (build.state === "succeeded") return "Healthy";

	// "Waiting for X" is only valid before the build has ever started.
	// build.startedAt is the authoritative signal — once it's set the build has
	// been running and any "queued" state from a later poll is stale backend lag,
	// not a real regression. Showing "Waiting for build" after "Building" is the
	// flicker the user sees; suppressing it here keeps the label monotonic.
	const hasStarted = Boolean(build.startedAt);
	if (build.state === "queued" && !hasStarted) {
		const pendingStage = nextPendingStage(build.stages ?? []);
		if (pendingStage?.label)
			return `Waiting for ${pendingStage.label.toLowerCase()}`;
		return "Queued";
	}

	if (build.state === "running" || build.state === "queued") return "Building";

	return "Offline";
}

function deploymentStageHealth(
	stages: Array<{ state: DashboardDeploymentStageState }>,
): "active" | "failed" | "complete" | "unknown" {
	const visibleStages = stages.filter((stage) => stage.state !== "skipped");
	if (visibleStages.some((stage) => stage.state === "failed")) return "failed";
	if (
		visibleStages.some(
			(stage) => stage.state === "running" || stage.state === "pending",
		)
	) {
		return "active";
	}
	if (
		visibleStages.length > 0 &&
		visibleStages.every((stage) => stage.state === "succeeded")
	) {
		return "complete";
	}
	return "unknown";
}

function nextPendingStage(stages: DashboardDeploymentStage[]) {
	const firstOpenStage = stages.find(
		(stage) => stage.state === "running" || stage.state === "pending",
	);
	return firstOpenStage?.state === "pending" ? firstOpenStage : undefined;
}

export function shortSha(sha: string): string {
	return sha.slice(0, 7);
}

export function shortId(id: string): string {
	return id.slice(0, 8);
}

export function buildBadgeClass(
	state: string,
): "healthy" | "building" | "failed" | "offline" {
	switch (state) {
		case "succeeded":
			return "healthy";
		case "running":
		case "queued":
			return "building";
		case "failed":
			return "failed";
		default:
			return "offline";
	}
}

export function buildServiceURL(
	state: DashboardHomeState,
	hostname: string,
): string {
	const base =
		state.localIngressBaseURL &&
		state.localDomainSuffix &&
		hostname.toLowerCase().endsWith(`.${state.localDomainSuffix.toLowerCase()}`)
			? state.localIngressBaseURL
			: state.publicBaseURL;
	try {
		const url = new URL(base);
		url.hostname = hostname;
		if (
			(url.protocol === "https:" && url.port === "443") ||
			(url.protocol === "http:" && url.port === "80")
		) {
			url.port = "";
		}
		return url.toString().replace(/\/$/, "");
	} catch {
		return `https://${hostname}`;
	}
}

export function formatError(error: unknown): string {
	if (error && typeof error === "object" && "message" in error) {
		return String((error as { message: unknown }).message);
	}
	return "An unexpected error occurred.";
}
