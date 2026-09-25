// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { DashboardDeploymentRecord } from "#/lib/dashboard/core/types.server";

const fetch = vi.hoisted(() => vi.fn());
vi.mock("./server-fns", () => ({ fetchBuildAttempts: fetch }));

import { DeploymentDetails } from "./deployment-details";
import { deploymentBadgeLabel } from "./deployment-inline";

afterEach(cleanup);
const record: DashboardDeploymentRecord = {
	id: "d",
	isCurrent: true,
	rolloutGeneration: 1,
	sealedVersions: { TOKEN: 3 },
	build: {
		buildId: "b",
		state: "BUILD_STATE_RUNNING",
		commitSha: "abc123",
		imageDigest: "",
		failureReason: "",
		attemptCount: 2,
		attemptLimit: 3,
		cancelRequestedAt: new Date(),
	},
	artifact: {
		id: "a",
		kind: "build",
		imageRef: "image@sha256:abc",
		commitSha: "abc123",
	},
};
it("shows cancellation, retry history, and pinned versions without secret values", async () => {
	fetch.mockResolvedValue([
		{
			attemptNumber: 1,
			builderId: "worker",
			outcome: "worker_lost",
			detail: "Worker lease expired",
		},
	]);
	render(<DeploymentDetails record={record} serviceId="s" />);
	expect(deploymentBadgeLabel("DEPLOYMENT_STATE_BUILDING", record.build)).toBe(
		"Cancelling",
	);
	fireEvent.click(screen.getByText("Build attempts 2/3"));
	await screen.findByText("Worker lease expired");
	expect(fetch).toHaveBeenCalledWith({
		data: { serviceId: "s", buildId: "b" },
	});
	expect(screen.getByText("TOKEN: v3")).toBeTruthy();
});
it("keeps reuse visible after the rollout's reason changes", () => {
	render(
		<DeploymentDetails
			serviceId="s"
			record={{
				...record,
				status: {
					deploymentId: "d",
					causeKind: "DEPLOYMENT_CAUSE_KIND_UNSPECIFIED",
					causeId: "",
					detail: "",
					specRevision: 1,
					imageDigest: "",
					rolloutGeneration: 1,
					state: "DEPLOYMENT_STATE_ACTIVE",
					reasonCode: "HEALTHY",
					buildReused: true,
				},
			}}
		/>,
	);
	expect(
		screen.getByText(/Reusing image built for commit abc123/),
	).toBeTruthy();
	expect(screen.queryByText(/Build attempts/)).toBeNull();
});
