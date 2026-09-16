import { describe, expect, it } from "vitest";

import type {
	DashboardBuildState,
	DashboardBuildStatus,
	DashboardDeploymentStatus,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import {
	deploymentMeta,
	deploymentSubtitle,
	getDeploymentCardTone,
	hasActiveDeployment,
	sourceRevisionState,
} from "./panel-deployments-helpers";

describe("sourceRevisionState", () => {
	it("reports deployed when the latest commit is live", () => {
		const service = serviceRecord({
			lastSuccessfulCommitSha: "abc123",
			latestRevisionSha: "abc123",
		});
		expect(sourceRevisionState(service, true)).toEqual({
			state: "deployed",
			commitSha: "abc123",
		});
		expect(sourceRevisionState(service, false)).toEqual({
			state: "deployed",
			commitSha: "abc123",
		});
	});

	it("reports waiting when auto-deploy will pick up the commit", () => {
		const service = serviceRecord({
			lastSuccessfulCommitSha: "abc123",
			latestRevisionSha: "def456",
		});
		expect(sourceRevisionState(service, true)).toEqual({
			state: "waiting",
			commitSha: "def456",
		});
	});

	it("reports waiting while a manual build of the commit is running", () => {
		const service = serviceRecord({
			lastSuccessfulCommitSha: "abc123",
			latestRevisionSha: "def456",
			buildSha: "def456",
			buildState: "BUILD_STATE_RUNNING",
		});
		expect(sourceRevisionState(service, false)).toEqual({
			state: "waiting",
			commitSha: "def456",
		});
	});

	it("reports ignored when auto-deploy is off and nothing is building", () => {
		const service = serviceRecord({
			lastSuccessfulCommitSha: "abc123",
			latestRevisionSha: "def456",
			buildSha: "abc123",
			buildState: "BUILD_STATE_SUCCEEDED",
		});
		expect(sourceRevisionState(service, false)).toEqual({
			state: "ignored",
			commitSha: "def456",
		});
	});

	it("stays quiet when the newest build already tells the failure story", () => {
		const service = serviceRecord({
			latestRevisionSha: "def456",
			buildSha: "def456",
			buildState: "BUILD_STATE_FAILED",
		});
		expect(sourceRevisionState(service, true)).toBeUndefined();
	});

	it("stays quiet without an observed revision", () => {
		expect(sourceRevisionState(serviceRecord({}), true)).toBeUndefined();
	});
});

function serviceRecord(input: {
	lastSuccessfulCommitSha?: string;
	latestRevisionSha?: string;
	buildSha?: string;
	buildState?: DashboardBuildState;
}): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		name: "web",
		lastSuccessfulCommitSha: input.lastSuccessfulCommitSha,
		sourceSummary: input.latestRevisionSha
			? { latestRevision: { commitSha: input.latestRevisionSha } }
			: undefined,
		latestBuild: input.buildSha
			? {
					buildId: "build-1",
					state: input.buildState ?? "BUILD_STATE_SUCCEEDED",
					commitSha: input.buildSha,
					imageDigest: "",
					failureReason: "",
				}
			: undefined,
	};
}

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
