// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { ServicePanel } from "./service-panel";

const { doUpdateServiceMock } = vi.hoisted(() => ({
	doUpdateServiceMock: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	doUpdateService: doUpdateServiceMock,
	fetchServiceDeployments: vi.fn().mockResolvedValue([]),
	fetchServiceLogs: vi.fn().mockResolvedValue([]),
}));

beforeEach(() => {
	doUpdateServiceMock.mockReset();
});

afterEach(cleanup);

describe("ServicePanel rename", () => {
	it("returns the title to the normal view when clicking outside the rename field", () => {
		render(
			<ServicePanel
				service={service()}
				status={null}
				project={{ id: "project-1", name: "test-project", kind: "user" }}
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
		project: { id: "project-1", name: "test-project", kind: "user" },
		environments: [environment],
		environment,
		onboarding: {
			currentStep: "build",
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
		service: service(),
		serviceStatus: undefined,
		publicBaseURL: "https://dashboard.example.test",
		ingressTargetHost: "platform.example.test",
		domainBindings: [],
		controlPlaneReachable: true,
	};
}
