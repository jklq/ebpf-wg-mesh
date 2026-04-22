import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import type { ServiceHealth } from "./types";

export function serviceHealth(service: DashboardServiceRecord): ServiceHealth {
	const build = service.latestBuild;
	if (!build) return "offline";
	if (build.state === "running" || build.state === "queued") return "building";
	if (build.state === "failed") return "failed";
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

export function repositoryInspectionBlocker(
	state: DashboardHomeState,
): string | undefined {
	const inspection = state.repositoryInspection;
	if (!inspection) {
		return "Repository inspection did not return a result.";
	}
	if (inspection.accessState === "installation_required") {
		return state.githubInstallURL
			? "The GitHub App is not installed for this repository yet. Open the install flow, grant the repository, then check again."
			: "This repository needs a GitHub App installation or repository grant, but no install URL is configured.";
	}
	if (inspection.accessState !== "available") {
		return "Repository access is still blocked.";
	}
	if (inspection.dockerfileCandidates.length === 0) {
		return "No Dockerfile was detected on the default branch.";
	}
	return undefined;
}
