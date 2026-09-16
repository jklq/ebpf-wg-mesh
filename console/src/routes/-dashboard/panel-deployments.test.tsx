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
		serverFns.fetchServiceDeployments
			.mockReset()
			.mockResolvedValue([failedDeployment()]);
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
		expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
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
					action: "DEPLOYMENT_ACTION_RETRY",
					idempotencyKey: expect.any(String),
				}),
			});
		});
		expect(onRedeployed).toHaveBeenCalled();
	});

	it("still opens the full log from the failed stage", async () => {
		const { rerender } = render(
			<PanelDeployments
				service={failedService()}
				status={null}
				project={project()}
			/>,
		);

		fireEvent.click(await screen.findByRole("button", { name: "Full log" }));
		expect(screen.getByText("Deployment logs")).toBeTruthy();

		rerender(
			<PanelDeployments
				service={failedService()}
				status={null}
				project={undefined}
			/>,
		);
		expect(screen.queryByText("Deployment logs")).toBeNull();
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
		expect(screen.getByText("Healthy")).toBeTruthy();
		expect(
			screen.getByText("Deployment in progress: Publishing image"),
		).toBeTruthy();
		expect(screen.queryByText("Deployment successful")).toBeNull();
		expect(screen.getAllByRole("button", { name: "View logs" }).length).toBe(2);
		expect(screen.queryByText("Source")).toBeNull();
		expect(screen.queryByText("Post-deploy")).toBeNull();
		expect(screen.queryByText(/via (GitHub|dashboard|system)/)).toBeNull();

		const historyToggle = screen.getByRole("button", { name: /History/ });
		expect(historyToggle.textContent).toContain("1");
		expect(screen.queryByText("older worker")).toBeNull();
		fireEvent.click(historyToggle);
		expect(await screen.findByText("older worker")).toBeTruthy();
		for (const trigger of screen.getAllByRole("button", {
			name: "Deployment actions",
		})) {
			fireEvent.click(trigger);
		}
		expect(screen.getByRole("menuitem", { name: "Cancel" })).toBeTruthy();
		expect(screen.getByRole("menuitem", { name: "Rollback" })).toBeTruthy();
		expect(screen.queryByRole("menuitem", { name: "Redeploy" })).toBeNull();
	});

	it("shows an intentional empty state before the first deployment", async () => {
		serverFns.fetchServiceDeployments.mockResolvedValue([]);
		const service = rollingService({
			latestBuild: undefined,
			latestDeployment: undefined,
			pendingChanges: true,
		});

		render(
			<PanelDeployments service={service} status={null} project={project()} />,
		);

		expect(await screen.findByText("Nothing deployed yet")).toBeTruthy();
		expect(
			screen.getByText("Deploy your changes to create the first deployment."),
		).toBeTruthy();
	});

	it("labels history entries without times instead of measuring from 1970", async () => {
		serverFns.fetchServiceDeployments.mockResolvedValue([
			{
				id: "deploy-0",
				rolloutGeneration: 1,
				isCurrent: false,
				build: {
					buildId: "build-0",
					state: "BUILD_STATE_SUCCEEDED",
					commitSha: "ee55ff66aa",
					imageDigest: "sha256:older",
					failureReason: "",
					commitMessage: "older worker",
				},
				status: {
					deploymentId: "deploy-0",
					state: "DEPLOYMENT_STATE_COMPLETED",
					causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK",
					causeId: "github",
					reasonCode: "DEPLOYMENT_COMPLETED",
					detail: "Replaced",
					specRevision: 1,
					imageDigest: "sha256:older",
					rolloutGeneration: 1,
				},
			},
		]);
		const service = rollingService({
			latestBuild: undefined,
			latestDeployment: undefined,
		});

		render(
			<PanelDeployments service={service} status={null} project={project()} />,
		);

		fireEvent.click(await screen.findByRole("button", { name: /History/ }));
		expect(await screen.findByText("older worker")).toBeTruthy();
		expect(screen.getByText("Not deployed")).toBeTruthy();
		expect(document.body.textContent).not.toContain("1970");
	});

	it("keeps generation-zero staged configuration out of deployment history", async () => {
		const stagedStatus = {
			deploymentId: "deploy-staged",
			state: "DEPLOYMENT_STATE_STAGED" as const,
			causeKind: "DEPLOYMENT_CAUSE_KIND_USER" as const,
			causeId: "user-1",
			reasonCode: "SERVICE_STAGED",
			detail: "Configuration staged",
			specRevision: 1,
			imageDigest: "",
			rolloutGeneration: 0,
		};
		serverFns.fetchServiceDeployments.mockResolvedValue([
			{
				id: stagedStatus.deploymentId,
				rolloutGeneration: 0,
				isCurrent: true,
				status: stagedStatus,
			},
		]);
		const service = rollingService({
			rolloutGeneration: 0,
			latestBuild: undefined,
			latestDeployment: stagedStatus,
			pendingChanges: true,
		});

		render(
			<PanelDeployments service={service} status={null} project={project()} />,
		);

		expect(await screen.findByText("Nothing deployed yet")).toBeTruthy();
		expect(screen.queryByText(/Configuration staged/)).toBeNull();
	});

	it("refreshes empty history when live status announces a deployment", async () => {
		serverFns.fetchServiceDeployments
			.mockResolvedValueOnce([])
			.mockResolvedValueOnce([buildingDeployment()]);
		const initialService = rollingService({
			latestBuild: undefined,
			latestDeployment: undefined,
		});
		const { rerender } = render(
			<PanelDeployments
				service={initialService}
				status={null}
				project={project()}
			/>,
		);
		await screen.findByText("Nothing deployed yet");

		rerender(
			<PanelDeployments
				service={initialService}
				status={rollingStatus()}
				project={project()}
			/>,
		);

		expect(await screen.findByText("Building")).toBeTruthy();
		expect(serverFns.fetchServiceDeployments).toHaveBeenCalledTimes(2);
	});

	it("shows the live building deployment before history catches up", () => {
		serverFns.fetchServiceDeployments.mockReturnValue(new Promise(() => {}));
		const service = rollingService({ latestBuild: undefined });

		render(
			<PanelDeployments service={service} status={null} project={project()} />,
		);

		expect(screen.getByText("Building")).toBeTruthy();
		expect(screen.getByRole("img", { name: "Deploy steps" })).toBeTruthy();
		expect(
			screen.getByText("Deployment in progress: Publishing image"),
		).toBeTruthy();
	});

	it("puts active deployment actions in a menu with remove last", async () => {
		const currentDeployment = { ...activeDeployment(), isCurrent: true };
		const service: DashboardServiceRecord = {
			...rollingService({
				rolloutGeneration: currentDeployment.rolloutGeneration,
				latestBuild: currentDeployment.build,
				latestDeployment: currentDeployment.status,
			}),
		};
		serverFns.fetchServiceDeployments.mockResolvedValue([currentDeployment]);

		render(
			<PanelDeployments
				service={service}
				status={{
					service,
					allocations: [
						allocation({ allocationId: "alloc-1" }),
						allocation({ allocationId: "alloc-2" }),
					],
				}}
				project={project()}
			/>,
		);

		fireEvent.click(
			await screen.findByRole("button", { name: "Deployment actions" }),
		);
		const menuItems = screen.getAllByRole("menuitem");
		expect(menuItems.map((item) => item.textContent)).toEqual([
			"Restart",
			"Redeploy",
			"Remove",
		]);
		expect(screen.queryByText("Redeploy exact")).toBeNull();
		expect(screen.getByLabelText("Restart target")).toBeTruthy();
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
						ownershipState: "DOMAIN_OWNERSHIP_STATE_VERIFIED",
					},
				]}
			/>,
		);

		expect(await screen.findByText("worker.example.com")).toBeTruthy();
		expect(screen.getByText("3 Replicas")).toBeTruthy();
	});

	it("shows a removed current deployment in history with a green status", async () => {
		const active = activeDeployment();
		if (!active.status) throw new Error("active deployment status is required");
		const removed: DashboardDeploymentRecord = {
			...active,
			isCurrent: true,
			status: {
				...active.status,
				state: "DEPLOYMENT_STATE_REMOVED" as const,
				reasonCode: "DEPLOYMENT_REMOVED",
				detail: "Service removed",
			},
		};
		const service = rollingService({
			rolloutGeneration: removed.rolloutGeneration,
			latestBuild: removed.build,
			latestDeployment: removed.status,
		});
		serverFns.fetchServiceDeployments.mockResolvedValue([removed]);

		render(
			<PanelDeployments service={service} status={null} project={project()} />,
		);

		const historyToggle = await screen.findByRole("button", {
			name: /History/,
		});
		expect(historyToggle.textContent).toContain("1");
		expect(screen.queryByRole("button", { name: "View logs" })).toBeNull();

		fireEvent.click(historyToggle);
		expect(screen.getByLabelText("Status: healthy")).toBeTruthy();
	});
});

function project(): DashboardProject {
	return { id: "project-1", name: "test-project", kind: "PROJECT_KIND_USER" };
}

function failedDeployment(): DashboardDeploymentRecord {
	const startedAt = new Date("2026-08-13T10:00:00Z");
	const finishedAt = new Date("2026-08-13T10:00:22Z");
	const build: DashboardBuildStatus = {
		buildId: "build-1",
		state: "BUILD_STATE_FAILED",
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
				state: "DEPLOYMENT_STAGE_STATE_FAILED",
				startedAt,
				finishedAt,
			},
			{
				key: "deploy",
				label: "Deploy",
				detail: "Waiting for build to finish",
				state: "DEPLOYMENT_STAGE_STATE_PENDING",
			},
			{
				key: "post-deploy",
				label: "Post-deploy",
				detail: "Waiting for rollout",
				state: "DEPLOYMENT_STAGE_STATE_PENDING",
			},
		],
	};
	return {
		id: "deploy-1",
		rolloutGeneration: 1,
		createdAt: startedAt,
		build,
		isCurrent: true,
		status: {
			deploymentId: "deploy-1",
			state: "DEPLOYMENT_STATE_FAILED",
			causeKind: "DEPLOYMENT_CAUSE_KIND_USER",
			causeId: "user-1",
			reasonCode: "build_failed",
			detail: "Build failed",
			specRevision: 1,
			imageDigest: "",
			rolloutGeneration: 1,
		},
	};
}

function failedService(
	overrides: Partial<DashboardServiceRecord> = {},
): DashboardServiceRecord {
	const deployment = failedDeployment();
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
		latestBuild: deployment.build,
		latestDeployment: deployment.status,
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
		state: "BUILD_STATE_RUNNING",
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
				state: "DEPLOYMENT_STAGE_STATE_SUCCEEDED",
				startedAt,
				finishedAt: startedAt,
			},
			{
				key: "build",
				label: "Build",
				detail: "Publishing image",
				state: "DEPLOYMENT_STAGE_STATE_RUNNING",
				startedAt,
			},
			{
				key: "deploy",
				label: "Deploy",
				detail: "Waiting for build to finish",
				state: "DEPLOYMENT_STAGE_STATE_PENDING",
			},
			{
				key: "post-deploy",
				label: "Post-deploy",
				detail: "Waiting for rollout",
				state: "DEPLOYMENT_STAGE_STATE_PENDING",
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
			state: "DEPLOYMENT_STATE_BUILDING",
			causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK",
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
			state: "BUILD_STATE_SUCCEEDED",
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
					state: "DEPLOYMENT_STAGE_STATE_SUCCEEDED",
					startedAt,
					finishedAt: startedAt,
				},
			],
		},
		isCurrent: false,
		status: {
			deploymentId: "deploy-1",
			state: "DEPLOYMENT_STATE_ACTIVE",
			causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK",
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
			state: "BUILD_STATE_SUCCEEDED",
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
			state: "DEPLOYMENT_STATE_COMPLETED",
			causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK",
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
		allocationIpv4: "10.200.0.2",
		allocationIpv6: "fd00:200::2",
		healthy: true,
		desiredRolloutGeneration: 2,
		appliedRolloutGeneration: 2,
		healthyIpv4Ports: [8080],
		healthyIpv6Ports: [8080],
		...overrides,
	};
}
