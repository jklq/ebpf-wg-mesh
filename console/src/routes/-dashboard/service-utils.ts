import type {
	DashboardDeploymentStageState,
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { NOT_DEPLOYED_LABEL } from "#/lib/time";

import { deploymentBadgeLabel } from "./deployment-inline";
import type { ServiceHealth } from "./types";

export function serviceHealth(service: DashboardServiceRecord): ServiceHealth {
	const deployment = service.latestDeployment;
	if (deployment) {
		if (
			deployment.state === "DEPLOYMENT_STATE_STAGED" &&
			deployment.rolloutGeneration === 0
		) {
			return "offline";
		}
		switch (deployment.state) {
			case "DEPLOYMENT_STATE_ACTIVE":
			case "DEPLOYMENT_STATE_DRAINING":
			case "DEPLOYMENT_STATE_COMPLETED":
				return "healthy";
			case "DEPLOYMENT_STATE_FAILED":
			case "DEPLOYMENT_STATE_CRASHED":
			case "DEPLOYMENT_STATE_CANCELLED":
				return "failed";
			case "DEPLOYMENT_STATE_REMOVED":
			case "DEPLOYMENT_STATE_SUPERSEDED":
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
	if (build.state === "BUILD_STATE_FAILED") return "failed";
	if (
		build.state === "BUILD_STATE_RUNNING" ||
		build.state === "BUILD_STATE_QUEUED"
	)
		return "building";
	if (build.state === "BUILD_STATE_SUCCEEDED") return "healthy";
	return "offline";
}

export function serviceStatusLabel(service: DashboardServiceRecord): string {
	const deployment = service.latestDeployment;
	if (
		deployment?.state === "DEPLOYMENT_STATE_STAGED" &&
		deployment.rolloutGeneration === 0
	) {
		return NOT_DEPLOYED_LABEL;
	}
	return deploymentBadgeLabel(deployment?.state, service.latestBuild);
}

function deploymentStageHealth(
	stages: Array<{ state: DashboardDeploymentStageState }>,
): "active" | "failed" | "complete" | "unknown" {
	const visibleStages = stages.filter(
		(stage) => stage.state !== "DEPLOYMENT_STAGE_STATE_SKIPPED",
	);
	if (
		visibleStages.some(
			(stage) => stage.state === "DEPLOYMENT_STAGE_STATE_FAILED",
		)
	)
		return "failed";
	if (
		visibleStages.some(
			(stage) =>
				stage.state === "DEPLOYMENT_STAGE_STATE_RUNNING" ||
				stage.state === "DEPLOYMENT_STAGE_STATE_PENDING",
		)
	) {
		return "active";
	}
	if (
		visibleStages.length > 0 &&
		visibleStages.every(
			(stage) => stage.state === "DEPLOYMENT_STAGE_STATE_SUCCEEDED",
		)
	) {
		return "complete";
	}
	return "unknown";
}

export function shortSha(sha: string): string {
	return sha.slice(0, 7);
}

export function shortId(id: string): string {
	return id.slice(0, 8);
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
