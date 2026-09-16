import { describe, expect, it } from "vitest";

import type {
	DashboardDeploymentState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { serviceHealth, serviceStatusLabel } from "./service-utils";

function serviceWith(
	state: DashboardDeploymentState,
	rolloutGeneration = 3,
): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		name: "hello",
		rolloutGeneration,
		latestDeployment: {
			deploymentId: "deploy-1",
			state,
			causeKind: "DEPLOYMENT_CAUSE_KIND_USER",
			causeId: "user-1",
			reasonCode: "TEST",
			detail: "",
			specRevision: 1,
			imageDigest: "",
			rolloutGeneration,
		},
	};
}

describe("serviceStatusLabel", () => {
	it("labels a newly staged service as not deployed", () => {
		expect(serviceStatusLabel(serviceWith("DEPLOYMENT_STATE_STAGED", 0))).toBe(
			"Not deployed",
		);
		expect(
			serviceStatusLabel({
				id: "service-1",
				environmentId: "environment-1",
				name: "hello",
			}),
		).toBe("Not deployed");
	});

	it("distinguishes every user-visible service state", () => {
		const cases: Array<[DashboardDeploymentState, string]> = [
			["DEPLOYMENT_STATE_STAGED", "Staged"],
			["DEPLOYMENT_STATE_QUEUED_BUILD", "Queued"],
			["DEPLOYMENT_STATE_BUILDING", "Building"],
			["DEPLOYMENT_STATE_SCHEDULING", "Deploying"],
			["DEPLOYMENT_STATE_IMAGE_PULL", "Deploying"],
			["DEPLOYMENT_STATE_STARTING", "Deploying"],
			["DEPLOYMENT_STATE_READINESS", "Deploying"],
			["DEPLOYMENT_STATE_ACTIVE", "Healthy"],
			["DEPLOYMENT_STATE_FAILED", "Unhealthy"],
			["DEPLOYMENT_STATE_CRASHED", "Crashed"],
			["DEPLOYMENT_STATE_CANCELLED", "Cancelled"],
			["DEPLOYMENT_STATE_REMOVED", "Removed"],
			["DEPLOYMENT_STATE_SUPERSEDED", "Superseded"],
		];
		for (const [state, label] of cases) {
			expect(serviceStatusLabel(serviceWith(state))).toBe(label);
		}
	});
});

describe("serviceHealth", () => {
	it("keeps tones aligned with the status labels", () => {
		expect(serviceHealth(serviceWith("DEPLOYMENT_STATE_ACTIVE"))).toBe(
			"healthy",
		);
		expect(serviceHealth(serviceWith("DEPLOYMENT_STATE_BUILDING"))).toBe(
			"building",
		);
		expect(serviceHealth(serviceWith("DEPLOYMENT_STATE_SCHEDULING"))).toBe(
			"building",
		);
		expect(serviceHealth(serviceWith("DEPLOYMENT_STATE_FAILED"))).toBe(
			"failed",
		);
		expect(serviceHealth(serviceWith("DEPLOYMENT_STATE_CRASHED"))).toBe(
			"failed",
		);
		expect(serviceHealth(serviceWith("DEPLOYMENT_STATE_STAGED", 0))).toBe(
			"offline",
		);
		expect(serviceHealth(serviceWith("DEPLOYMENT_STATE_REMOVED"))).toBe(
			"offline",
		);
	});
});
