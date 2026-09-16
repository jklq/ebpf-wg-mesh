import { describe, expect, it } from "vitest";

import type {
	DashboardBuildStatus,
	DashboardDeploymentStatus,
} from "#/lib/dashboard/core/types.server";

import {
	deploymentMeta,
	deploymentSubtitle,
	getDeploymentCardTone,
	hasActiveDeployment,
} from "./panel-deployments-helpers";

const NOW_MS = new Date("2026-08-13T10:05:00.000Z").getTime();

function buildWith(
	timestamps: Partial<
		Pick<DashboardBuildStatus, "queuedAt" | "startedAt" | "finishedAt">
	>,
): DashboardBuildStatus {
	return {
		buildId: "build-1",
		state: "BUILD_STATE_RUNNING",
		commitSha: "abc1234def",
		imageDigest: "",
		failureReason: "",
		...timestamps,
	};
}

describe("deploymentSubtitle", () => {
	it("labels a staged service without times as not deployed", () => {
		expect(deploymentSubtitle(undefined, 0, NOW_MS)).toBe("Not deployed");
		expect(deploymentSubtitle(undefined, undefined, NOW_MS)).toBe(
			"Not deployed",
		);
	});

	it("keeps the rollout fallback for deployed generations without times", () => {
		expect(deploymentSubtitle(undefined, 3, NOW_MS)).toBe("Rollout 3");
	});

	it("defends against future clock skew", () => {
		expect(
			deploymentSubtitle(
				buildWith({ startedAt: new Date(NOW_MS + 60_000) }),
				3,
				NOW_MS,
			),
		).toBe("just now");
	});

	it("formats valid build times relatively", () => {
		expect(
			deploymentSubtitle(
				buildWith({ startedAt: new Date(NOW_MS - 5 * 60_000) }),
				3,
				NOW_MS,
			),
		).toBe("5 minutes ago");
	});
});

describe("deploymentMeta", () => {
	it("labels missing timestamps instead of omitting them", () => {
		expect(deploymentMeta(undefined, undefined, NOW_MS)).toEqual([
			"Not deployed",
		]);
	});

	it("formats valid timestamps relatively", () => {
		expect(
			deploymentMeta(buildWith({}), new Date(NOW_MS - 5_000), NOW_MS),
		).toEqual(["abc1234", "5 seconds ago"]);
	});
});

function statusWith(
	state: DashboardDeploymentStatus["state"],
): DashboardDeploymentStatus {
	return { state } as DashboardDeploymentStatus;
}

describe("hasActiveDeployment", () => {
	it("treats an unspecified state as active", () => {
		expect(
			hasActiveDeployment(
				statusWith("DEPLOYMENT_STATE_UNSPECIFIED"),
				undefined,
			),
		).toBe(true);
	});

	it("treats a removed deployment as inactive", () => {
		expect(
			hasActiveDeployment(statusWith("DEPLOYMENT_STATE_REMOVED"), undefined),
		).toBe(false);
	});
});

describe("getDeploymentCardTone", () => {
	it("renders a removed deployment as draining", () => {
		expect(
			getDeploymentCardTone({
				build: undefined,
				allocation: undefined,
				active: false,
				isCurrent: false,
				status: statusWith("DEPLOYMENT_STATE_REMOVED"),
			}),
		).toBe("draining");
	});
});
