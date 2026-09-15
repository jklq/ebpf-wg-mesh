import { describe, expect, it } from "vitest";

import type {
	DashboardBuildState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { sourceRevisionState } from "./panel-deployments-helpers";

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
