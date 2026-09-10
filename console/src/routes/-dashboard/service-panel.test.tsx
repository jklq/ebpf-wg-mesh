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
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { ServicePanelFallback } from "./dashboard-canvas";
import { ServicePanel } from "./service-panel";

const { doScaleServiceMock, doUpdateServiceMock } = vi.hoisted(() => ({
	doScaleServiceMock: vi.fn(),
	doUpdateServiceMock: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	doUpdateService: doUpdateServiceMock,
	doScaleService: doScaleServiceMock,
	fetchServiceDeployments: vi.fn().mockResolvedValue([]),
	fetchServiceLogs: vi.fn().mockResolvedValue([]),
	doApplyDeploymentAction: vi.fn(),
}));

beforeEach(() => {
	doScaleServiceMock.mockReset();
	doUpdateServiceMock.mockReset();
});

afterEach(cleanup);

describe("ServicePanel rename", () => {
	it("returns the title to the normal view when clicking outside the rename field", () => {
		render(
			<ServicePanel
				service={service()}
				status={null}
				project={{
					id: "project-1",
					name: "test-project",
					kind: "PROJECT_KIND_USER",
				}}
				state={state()}
				activeTab="deployments"
				onTabChange={() => {}}
				onClose={() => {}}
				onRefresh={() => {}}
				onServiceUpdated={() => {}}
				onServiceDeleted={() => {}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "hello" }));
		const input = screen.getByRole("textbox", { name: "Service name" });
		fireEvent.change(input, { target: { value: "renamed" } });
		expect(screen.queryByRole("button", { name: "hello" })).toBeNull();

		fireEvent.mouseDown(document.body);

		expect(screen.getByRole("button", { name: "hello" })).toBeTruthy();
		expect(screen.queryByRole("textbox", { name: "Service name" })).toBeNull();
		expect(doUpdateServiceMock).not.toHaveBeenCalled();
	});
});

describe("ServicePanel deployment badge", () => {
	it("does not pulse Building while the service has undeployed changes", () => {
		render(
			<ServicePanel
				service={{
					...service(),
					specRevision: 2,
					pendingChanges: true,
					unappliedChangeCount: 1,
					latestBuild: {
						buildId: "build-1",
						state: "BUILD_STATE_RUNNING",
						commitSha: "",
						imageDigest: "",
						failureReason: "",
					},
				}}
				status={{
					service: {
						...service(),
						specRevision: 1,
						latestBuild: {
							buildId: "build-1",
							state: "BUILD_STATE_RUNNING",
							commitSha: "",
							imageDigest: "",
							failureReason: "",
						},
					},
					allocations: [],
				}}
				project={{
					id: "project-1",
					name: "test-project",
					kind: "PROJECT_KIND_USER",
				}}
				state={state()}
				activeTab="settings"
				onTabChange={() => {}}
				onClose={() => {}}
				onRefresh={() => {}}
				onServiceUpdated={() => {}}
				onServiceDeleted={() => {}}
			/>,
		);

		expect(screen.queryByLabelText("Service status")).toBeNull();
	});
});

describe("ServicePanel replica scaling", () => {
	it("keeps replica controls off the header", () => {
		render(
			<ServicePanel
				service={{ ...service(), desiredReplicaCount: 1, readyReplicaCount: 1 }}
				status={null}
				project={{
					id: "project-1",
					name: "test-project",
					kind: "PROJECT_KIND_USER",
				}}
				state={state()}
				activeTab="deployments"
				onTabChange={() => {}}
				onClose={() => {}}
				onRefresh={() => {}}
				onServiceUpdated={() => {}}
				onServiceDeleted={() => {}}
			/>,
		);

		expect(screen.queryByRole("button", { name: "Scale up" })).toBeNull();
		expect(screen.queryByRole("button", { name: "Scale down" })).toBeNull();
	});

	it("queues a replica count without applying it live", async () => {
		doScaleServiceMock.mockResolvedValue({
			service: {
				...service(),
				desiredReplicaCount: 1,
				spec: { ...service().spec, desiredReplicaCount: 2 },
			},
		});
		const onServiceUpdated = vi.fn();
		render(
			<ServicePanel
				service={{ ...service(), desiredReplicaCount: 1, readyReplicaCount: 1 }}
				status={null}
				project={{
					id: "project-1",
					name: "test-project",
					kind: "PROJECT_KIND_USER",
				}}
				state={state()}
				activeTab="settings"
				onTabChange={() => {}}
				onClose={() => {}}
				onRefresh={() => {}}
				onServiceUpdated={onServiceUpdated}
				onServiceDeleted={() => {}}
			/>,
		);

		const input = await screen.findByLabelText("Current count");
		fireEvent.change(input, { target: { value: "2" } });
		await waitFor(() => {
			expect(doScaleServiceMock).toHaveBeenCalledWith({
				data: {
					serviceId: "service-1",
					desiredReplicaCount: 2,
				},
			});
		});
		expect(onServiceUpdated).toHaveBeenCalledWith(
			expect.objectContaining({
				desiredReplicaCount: 1,
				spec: expect.objectContaining({ desiredReplicaCount: 2 }),
			}),
		);
	});

	it("rejects a second replica on a volume-backed service", async () => {
		const current = service();
		const spec = current.spec;
		if (!spec) {
			throw new Error("expected service spec");
		}
		render(
			<ServicePanel
				service={{
					...current,
					desiredReplicaCount: 1,
					readyReplicaCount: 1,
					spec: {
						...spec,
						runtime: {
							...spec.runtime,
							volumeName: "data",
						},
					},
				}}
				status={null}
				project={{
					id: "project-1",
					name: "test-project",
					kind: "PROJECT_KIND_USER",
				}}
				state={state()}
				activeTab="settings"
				onTabChange={() => {}}
				onClose={() => {}}
				onRefresh={() => {}}
				onServiceUpdated={() => {}}
				onServiceDeleted={() => {}}
			/>,
		);

		const input = await screen.findByLabelText("Current count");
		fireEvent.change(input, { target: { value: "2" } });
		expect(doUpdateServiceMock).not.toHaveBeenCalled();
	});

	it("rejects a replica count of zero", async () => {
		doUpdateServiceMock.mockResolvedValue(service());
		render(
			<ServicePanel
				service={{ ...service(), desiredReplicaCount: 1, readyReplicaCount: 1 }}
				status={null}
				project={{
					id: "project-1",
					name: "test-project",
					kind: "PROJECT_KIND_USER",
				}}
				state={state()}
				activeTab="settings"
				onTabChange={() => {}}
				onClose={() => {}}
				onRefresh={() => {}}
				onServiceUpdated={() => {}}
				onServiceDeleted={() => {}}
			/>,
		);

		const input = await screen.findByLabelText("Current count");
		fireEvent.change(input, { target: { value: "0" } });
		expect(doUpdateServiceMock).not.toHaveBeenCalled();
		expect(screen.queryByText("Scale production to zero")).toBeNull();

		fireEvent.blur(input);
		expect(screen.getByText("Enter a whole number from 1 to 64")).toBeTruthy();
		expect(doUpdateServiceMock).not.toHaveBeenCalled();
	});
});

describe("ServicePanelFallback", () => {
	it("keeps a working close button while the panel chunk loads", () => {
		const onClose = vi.fn();
		render(
			<ServicePanelFallback
				service={service()}
				onClose={onClose}
				onRefresh={() => {}}
			/>,
		);

		fireEvent.click(
			screen.getByRole("button", { name: /close service panel/i }),
		);
		expect(onClose).toHaveBeenCalledTimes(1);
	});
});

function service(): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "hello",
		spec: {
			source: {
				provider: "github",
				repositorySelector: "octocat/hello",
				trackedRef: "main",
			},
			runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		},
	};
}

function state(): DashboardHomeState {
	const environment = {
		id: "environment-1",
		projectId: "project-1",
		name: "production",
		kind: "persistent" as const,
		isProduction: true,
	};
	return {
		user: { id: "user-1", email: "user@example.com" },
		project: {
			id: "project-1",
			name: "test-project",
			kind: "PROJECT_KIND_USER",
		},
		environments: [environment],
		environment,
		onboarding: {
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		services: [service()],
		servicesRevision: 0,
		selectedServiceId: null,
		publicBaseURL: "https://dashboard.example.test",
		ingressTargetHost: "platform.example.test",
		domainBindings: [],
		controlPlaneReachable: true,
	};
}
