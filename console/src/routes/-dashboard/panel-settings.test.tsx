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

import { PanelSettings } from "./panel-settings";

const { doDeleteServiceMock, doUpdateServiceMock } = vi.hoisted(() => ({
	doDeleteServiceMock: vi.fn(),
	doUpdateServiceMock: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	doDeleteService: doDeleteServiceMock,
	doUpdateService: doUpdateServiceMock,
}));

beforeEach(() => {
	doDeleteServiceMock.mockReset().mockResolvedValue(undefined);
	doUpdateServiceMock.mockReset();
});

afterEach(cleanup);

describe("PanelSettings", () => {
	it("hides the CPU and memory resource fields", () => {
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		expect(screen.queryByText(/CPU request/i)).toBeNull();
		expect(screen.queryByText(/Memory request/i)).toBeNull();
	});

	it("changes the source repository through the picker", async () => {
		const withRepos = state();
		withRepos.repositories = [
			{
				fullName: "octocat/other",
				owner: "octocat",
				name: "other",
				private: false,
				defaultBranch: "main",
			},
		];
		doUpdateServiceMock.mockResolvedValue(service());
		render(
			<PanelSettings
				service={service()}
				state={withRepos}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		expect(screen.getByText("octocat/hello")).toBeTruthy();
		expect(screen.queryByRole("combobox")).toBeNull();

		fireEvent.click(
			screen.getByRole("button", { name: /change source repository/i }),
		);
		fireEvent.click(screen.getByRole("button", { name: "octocat/other" }));

		expect(screen.getByText("octocat/other")).toBeTruthy();

		fireEvent.click(screen.getByRole("button", { name: /save changes/i }));
		await waitFor(() =>
			expect(doUpdateServiceMock).toHaveBeenCalledWith({
				data: expect.objectContaining({
					serviceId: "service-1",
					repositorySelector: "octocat/other",
				}),
			}),
		);
	});

	it("deletes the service once its name is typed back", async () => {
		const onDeleted = vi.fn();
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={onDeleted}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: /delete service/i }));

		const confirm = screen.getByRole("button", { name: "Delete" });
		expect((confirm as HTMLButtonElement).disabled).toBe(true);

		fireEvent.change(screen.getByLabelText("Type hello to confirm"), {
			target: { value: "hello" },
		});
		fireEvent.click(confirm);

		await waitFor(() =>
			expect(doDeleteServiceMock).toHaveBeenCalledWith({
				data: { serviceId: "service-1" },
			}),
		);
		expect(onDeleted).toHaveBeenCalledWith("service-1");
	});

	it("keeps the service when the delete request fails", async () => {
		doDeleteServiceMock.mockRejectedValue(new Error("service is protected"));
		const onDeleted = vi.fn();
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={onDeleted}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: /delete service/i }));
		fireEvent.change(screen.getByLabelText("Type hello to confirm"), {
			target: { value: "hello" },
		});
		fireEvent.click(screen.getByRole("button", { name: "Delete" }));

		expect(await screen.findByText("service is protected")).toBeTruthy();
		expect(onDeleted).not.toHaveBeenCalled();
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
