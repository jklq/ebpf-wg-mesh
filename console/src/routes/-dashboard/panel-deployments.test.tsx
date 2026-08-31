// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardDeploymentRecord,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { PanelDeployments } from "./panel-deployments";

const serverFns = vi.hoisted(() => ({
	fetchServiceDeployments: vi.fn(),
	fetchServiceLogs: vi.fn(),
	doApplyDeploymentAction: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	fetchServiceDeployments: serverFns.fetchServiceDeployments,
	fetchServiceLogs: serverFns.fetchServiceLogs,
	doApplyDeploymentAction: serverFns.doApplyDeploymentAction,
}));

describe("deployments panel inline failure", () => {
	afterEach(cleanup);

	beforeEach(() => {
		serverFns.fetchServiceDeployments.mockReset().mockResolvedValue([]);
		serverFns.fetchServiceLogs.mockReset().mockResolvedValue([
			{
				observedAt: new Date("2026-08-13T10:00:00Z"),
				allocationId: "",
				agentId: "builder-1",
				stream: "stdout",
				rolloutGeneration: 1,
				sequence: 1,
				line: "#6 [builder 5/7] RUN go build -o /out/worker ./cmd/worker",
				logType: "build",
				buildId: "build-1",
				stage: "build",
			},
			{
				observedAt: new Date("2026-08-13T10:00:01Z"),
				allocationId: "",
				agentId: "builder-1",
				stream: "stdout",
				rolloutGeneration: 1,
				sequence: 2,
				line: "#6 0.412 config: reading environment",
				logType: "build",
				buildId: "build-1",
				stage: "build",
			},
			{
				observedAt: new Date("2026-08-13T10:00:02Z"),
				allocationId: "",
				agentId: "builder-1",
				stream: "stderr",
				rolloutGeneration: 1,
				sequence: 3,
				line: "#6 0.418 fatal: STRIPE_KEY is required at build time",
				logType: "build",
				buildId: "build-1",
				stage: "build",
			},
			{
				observedAt: new Date("2026-08-13T10:00:03Z"),
				allocationId: "",
				agentId: "builder-1",
				stream: "stderr",
				rolloutGeneration: 1,
				sequence: 4,
				line: "#6 ERROR: process did not complete successfully: exit code 1",
				logType: "build",
				buildId: "build-1",
				stage: "build",
			},
		]);
		serverFns.doApplyDeploymentAction.mockReset().mockResolvedValue({
			service: failedService(),
		});
	});

	it("keeps the failing build lines on the stage instead of a drawer", async () => {
		render(
			<PanelDeployments
				service={failedService()}
				status={null}
				project={project()}
			/>,
		);

		expect(
			await screen.findByText(
				"#6 0.418 fatal: STRIPE_KEY is required at build time",
			),
		).toBeTruthy();
		expect(screen.queryByText(/not attempted/)).toBeNull();
		expect(screen.queryByText("Post-deploy")).toBeNull();
		expect(
			screen.getByText(
				"This first rollout never left the builder. Nothing is serving yet.",
			),
		).toBeTruthy();
		expect(document.querySelector(".deployment-log-drawer.open")).toBeNull();
	});

	it("names the last healthy sha when a later rollout dies in the builder", async () => {
		render(
			<PanelDeployments
				service={failedService({ lastSuccessfulCommitSha: "5b1c0afabc123" })}
				status={null}
				project={project()}
			/>,
		);

		expect(await screen.findByText("5b1c0af")).toBeTruthy();
		expect(screen.getByText(/Nothing was taken down/)).toBeTruthy();
	});

	it("hands a missing env key to the variables editor", async () => {
		const onOpenVariables = vi.fn();
		render(
			<PanelDeployments
				service={failedService()}
				status={null}
				project={project()}
				onOpenVariables={onOpenVariables}
			/>,
		);

		fireEvent.click(
			await screen.findByRole("button", { name: "Add STRIPE_KEY" }),
		);
		expect(onOpenVariables).toHaveBeenCalledWith("STRIPE_KEY");
	});

	it("retries the build from the failed stage", async () => {
		const onRedeployed = vi.fn();
		render(
			<PanelDeployments
				service={failedService()}
				status={null}
				project={project()}
				onRedeployed={onRedeployed}
			/>,
		);

		fireEvent.click(await screen.findByRole("button", { name: "Retry build" }));
		await waitFor(() => {
			expect(serverFns.doApplyDeploymentAction).toHaveBeenCalledWith({
				data: expect.objectContaining({
					serviceId: "service-1",
					deploymentId: "deploy-1",
					action: "retry",
					idempotencyKey: expect.any(String),
				}),
			});
		});
		expect(onRedeployed).toHaveBeenCalled();
	});

	it("still opens the full log from the failed stage", async () => {
		render(
			<PanelDeployments
				service={failedService()}
				status={null}
				project={project()}
			/>,
		);

		fireEvent.click(await screen.findByRole("button", { name: "Full log" }));
		expect(screen.getByText("Deployment logs")).toBeTruthy();
	});
});

describe("deployments panel live rollouts", () => {
	afterEach(cleanup);

	beforeEach(() => {
		serverFns.fetchServiceDeployments
			.mockReset()
			.mockResolvedValue([
				buildingDeployment(),
				activeDeployment(),
				completedDeployment(),
			]);
		serverFns.fetchServiceLogs.mockReset().mockResolvedValue([]);
		serverFns.doApplyDeploymentAction.mockReset();
	});

	it("keeps the old active deployment live during a rolling release", async () => {
		render(
			<PanelDeployments
				service={rollingService()}
				status={rollingStatus()}
				project={project()}
			/>,
		);

		expect(await screen.findByText("Building")).toBeTruthy();
		expect(screen.getByText("Active")).toBeTruthy();
		expect(
			screen.getByText("Deployment in progress: Publishing image"),
		).toBeTruthy();
		expect(screen.queryByText("Deployment successful")).toBeNull();
		expect(screen.getAllByRole("button", { name: "View logs" }).length).toBe(2);
		expect(document.querySelector(".tone-active")).toBeTruthy();
		expect(screen.queryByText("Source")).toBeNull();
		expect(screen.queryByText("Post-deploy")).toBeNull();

		const historyToggle = screen.getByRole("button", { name: /History/ });
		expect(historyToggle.textContent).toContain("1");
		expect(screen.queryByText("older worker")).toBeNull();
		fireEvent.click(historyToggle);
		expect(await screen.findByText("older worker")).toBeTruthy();
		expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
		expect(screen.getByRole("button", { name: "Rollback" })).toBeTruthy();
	});

	it("summarizes exposure and replicas above the deployment cards", async () => {
		render(
			<PanelDeployments
				service={rollingService({ desiredReplicaCount: 3 })}
				status={rollingStatus()}
				project={project()}
				domains={[
					{
						hostname: "worker.example.com",
						serviceId: "service-1",
						targetPort: 8080,
						platformGenerated: false,
						ownershipState: "verified",
					},
				]}
			/>,
		);

		expect(await screen.findByText("worker.example.com")).toBeTruthy();
		expect(screen.getByText("3 Replicas")).toBeTruthy();
		expect(document.querySelector(".replica-rollout")).toBeNull();
	});
});

function project(): DashboardProject {
	return { id: "project-1", name: "test-project", kind: "user" };
}

function failedService(
	overrides: Partial<DashboardServiceRecord> = {},
): DashboardServiceRecord {
	const startedAt = new Date("2026-08-13T10:00:00Z");
	const finishedAt = new Date("2026-08-13T10:00:22Z");
	const build: DashboardBuildStatus = {
		buildId: "build-1",
		state: "failed",
		commitSha: "0c19aa4deadbeef",
		imageDigest: "",
		failureReason: "STRIPE_KEY is required at build time",
		commitMessage: "charge the invoice worker",
		queuedAt: startedAt,
		startedAt,
		finishedAt,
		stages: [
			{
				key: "build",
				label: "Build",
				detail: "Build failed",
				state: "failed",
				startedAt,
				finishedAt,
			},
			{
				key: "deploy",
				label: "Deploy",
				detail: "Waiting for build to finish",
				state: "pending",
			},
			{
				key: "post-deploy",
				label: "Post-deploy",
				detail: "Waiting for rollout",
				state: "pending",
			},
		],
	};
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "billing-worker",
		spec: {
			source: {
				provider: "github",
				repositorySelector: "relay5/billing-worker",
				trackedRef: "main",
			},
			runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		},
		latestBuild: build,
		latestDeployment: {
			deploymentId: "deploy-1",
			state: "failed",
			causeKind: "user",
			causeId: "user-1",
			reasonCode: "build_failed",
			detail: "Build failed",
			specRevision: 1,
			imageDigest: "",
			rolloutGeneration: 1,
		},
		...overrides,
	};
}

function rollingService(
	overrides: Partial<DashboardServiceRecord> = {},
): DashboardServiceRecord {
	const incoming = buildingDeployment();
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "billing-worker",
		desiredReplicaCount: 3,
		readyReplicaCount: 1,
		spec: {
			source: {
				provider: "github",
				repositorySelector: "relay5/billing-worker",
				trackedRef: "main",
			},
			runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		},
		latestBuild: incoming.build,
		latestDeployment: incoming.status,
		...overrides,
	};
}

function rollingStatus(): DashboardServiceStatus {
	return {
		service: rollingService(),
		allocations: [
			allocation({
				allocationId: "alloc-old-1",
				healthy: true,
				appliedRolloutGeneration: 1,
				desiredRolloutGeneration: 1,
			}),
			allocation({
				allocationId: "alloc-new-1",
				healthy: true,
				appliedRolloutGeneration: 2,
				desiredRolloutGeneration: 2,
			}),
			allocation({
				allocationId: "alloc-new-2",
				healthy: false,
				phase: "Starting",
				appliedRolloutGeneration: 1,
				desiredRolloutGeneration: 2,
			}),
		],
	};
}

function buildingDeployment(): DashboardDeploymentRecord {
	const startedAt = new Date("2026-08-13T10:05:00Z");
	const build: DashboardBuildStatus = {
		buildId: "build-2",
		state: "running",
		commitSha: "aa11bb22cc",
		imageDigest: "",
		failureReason: "",
		commitMessage: "Merge pull request #1 from railwayapp-te",
		queuedAt: startedAt,
		startedAt,
		stages: [
			{
				key: "source",
				label: "Source",
				detail: "relay5/billing-worker",
				state: "succeeded",
				startedAt,
				finishedAt: startedAt,
			},
			{
				key: "build",
				label: "Build",
				detail: "Publishing image",
				state: "running",
				startedAt,
			},
			{
				key: "deploy",
				label: "Deploy",
				detail: "Waiting for build to finish",
				state: "pending",
			},
			{
				key: "post-deploy",
				label: "Post-deploy",
				detail: "Waiting for rollout",
				state: "pending",
			},
		],
	};
	return {
		id: "deploy-2",
		rolloutGeneration: 2,
		createdAt: startedAt,
		build,
		isCurrent: true,
		status: {
			deploymentId: "deploy-2",
			state: "building",
			causeKind: "webhook",
			causeId: "github",
			reasonCode: "BUILD_STARTED",
			detail: "Publishing image",
			specRevision: 2,
			imageDigest: "",
			rolloutGeneration: 2,
			transitionedAt: startedAt,
		},
	};
}

function pinnedDigest(nibble: string): string {
	return `registry.example.test/web@sha256:${nibble.repeat(64)}`;
}

function activeDeployment(): DashboardDeploymentRecord {
	const startedAt = new Date("2026-08-13T10:02:00Z");
	const imageDigest = pinnedDigest("a");
	return {
		id: "deploy-1",
		rolloutGeneration: 1,
		createdAt: startedAt,
		build: {
			buildId: "build-1",
			state: "succeeded",
			commitSha: "cc33dd44ee",
			imageDigest,
			failureReason: "",
			commitMessage: "keep serving invoices",
			queuedAt: startedAt,
			startedAt,
			finishedAt: startedAt,
			stages: [
				{
					key: "deploy",
					label: "Deploy",
					detail: "Serving traffic",
					state: "succeeded",
					startedAt,
					finishedAt: startedAt,
				},
			],
		},
		isCurrent: false,
		status: {
			deploymentId: "deploy-1",
			state: "active",
			causeKind: "webhook",
			causeId: "github",
			reasonCode: "DEPLOYMENT_ACTIVE",
			detail: "Serving traffic",
			specRevision: 1,
			imageDigest,
			rolloutGeneration: 1,
			transitionedAt: startedAt,
		},
	};
}

function completedDeployment(): DashboardDeploymentRecord {
	const startedAt = new Date("2026-08-13T09:00:00Z");
	return {
		id: "deploy-0",
		rolloutGeneration: 0,
		createdAt: startedAt,
		build: {
			buildId: "build-0",
			state: "succeeded",
			commitSha: "ee55ff66aa",
			imageDigest: "sha256:older",
			failureReason: "",
			commitMessage: "older worker",
			queuedAt: startedAt,
			startedAt,
			finishedAt: startedAt,
		},
		isCurrent: false,
		status: {
			deploymentId: "deploy-0",
			state: "completed",
			causeKind: "webhook",
			causeId: "github",
			reasonCode: "DEPLOYMENT_COMPLETED",
			detail: "Replaced",
			specRevision: 1,
			imageDigest: "sha256:older",
			rolloutGeneration: 0,
			transitionedAt: startedAt,
		},
	};
}

function allocation(
	overrides: Partial<DashboardAllocationStatus>,
): DashboardAllocationStatus {
	return {
		allocationId: "alloc",
		serviceId: "service-1",
		agentId: "agent-1",
		desiredSpecRevision: 2,
		appliedSpecRevision: 2,
		phase: "Running",
		message: "",
		allocationIp: "",
		healthy: true,
		desiredRolloutGeneration: 2,
		appliedRolloutGeneration: 2,
		healthyPorts: [8080],
		...overrides,
	};
}
