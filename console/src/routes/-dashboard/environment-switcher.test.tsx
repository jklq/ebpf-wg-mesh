// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

import { EnvironmentSwitcher } from "./environment-switcher";

const {
	doRenameEnvironmentMock,
	doDeleteEnvironmentMock,
	doUpdateEnvironmentAutoDeployMock,
} = vi.hoisted(() => ({
	doRenameEnvironmentMock: vi.fn(),
	doDeleteEnvironmentMock: vi.fn(),
	doUpdateEnvironmentAutoDeployMock: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	fetchDeletionPreview: vi.fn().mockResolvedValue({
		environments: [],
		services: [],
		domains: [],
		volumes: [],
	}),
	doRenameEnvironment: doRenameEnvironmentMock,
	doDeleteResource: doDeleteEnvironmentMock,
	doUpdateEnvironmentAutoDeploy: doUpdateEnvironmentAutoDeployMock,
}));

beforeEach(() => {
	doRenameEnvironmentMock.mockReset().mockResolvedValue(undefined);
	doDeleteEnvironmentMock.mockReset().mockResolvedValue(undefined);
	doUpdateEnvironmentAutoDeployMock.mockReset().mockResolvedValue(undefined);
});

afterEach(cleanup);

describe("EnvironmentSwitcher", () => {
	it("renames an environment inline from the row menu", async () => {
		const onChanged = vi.fn();
		render(
			<EnvironmentSwitcher
				state={state()}
				onCreateEnvironment={() => {}}
				onChanged={onChanged}
				onNavigateEnvironment={() => {}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Environment" }));
		fireEvent.click(
			screen.getByRole("button", { name: /environment actions for staging/i }),
		);
		fireEvent.click(screen.getByRole("menuitem", { name: "Rename" }));

		const input = screen.getByRole("textbox", { name: /rename staging/i });
		expect(document.activeElement).toBe(input);
		expect((input as HTMLInputElement).selectionStart).toBe(0);
		expect((input as HTMLInputElement).selectionEnd).toBe("staging".length);
		fireEvent.change(input, { target: { value: "preview" } });
		fireEvent.submit(input);

		await waitFor(() =>
			expect(doRenameEnvironmentMock).toHaveBeenCalledWith({
				data: { environmentId: "environment-2", name: "preview" },
			}),
		);
		expect(onChanged).toHaveBeenCalled();
	});

	it("deletes a non-production environment after confirmation", async () => {
		render(
			<EnvironmentSwitcher
				state={state()}
				onCreateEnvironment={() => {}}
				onChanged={() => {}}
				onNavigateEnvironment={() => {}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Environment" }));
		fireEvent.click(
			screen.getByRole("button", { name: /environment actions for staging/i }),
		);
		fireEvent.click(screen.getByRole("menuitem", { name: "Delete" }));

		const confirm = screen.getByRole("button", { name: "Delete" });
		await waitFor(() =>
			expect((confirm as HTMLButtonElement).disabled).toBe(false),
		);
		fireEvent.click(confirm);

		await waitFor(() =>
			expect(doDeleteEnvironmentMock).toHaveBeenCalledWith({
				data: {
					kind: "environment",
					id: "environment-2",
					confirmationName: "",
				},
			}),
		);
	});

	it("renders the delete dialog above the whole viewport and closes only it", () => {
		render(
			<EnvironmentSwitcher
				state={state()}
				onCreateEnvironment={() => {}}
				onChanged={() => {}}
				onNavigateEnvironment={() => {}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Environment" }));
		fireEvent.click(
			screen.getByRole("button", { name: /environment actions for staging/i }),
		);
		fireEvent.click(screen.getByRole("menuitem", { name: "Delete" }));

		const dialog = screen.getByRole("dialog", { name: "Delete Environment" });
		expect(dialog.parentElement).toBe(document.body);
		fireEvent.keyDown(dialog, { key: "Escape" });

		expect(
			screen.queryByRole("dialog", { name: "Delete Environment" }),
		).toBeNull();
	});

	it("offers deletion for production with a separate confirmation", () => {
		render(
			<EnvironmentSwitcher
				state={state()}
				onCreateEnvironment={() => {}}
				onChanged={() => {}}
				onNavigateEnvironment={() => {}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Environment" }));
		fireEvent.click(
			screen.getByRole("button", {
				name: /environment actions for production/i,
			}),
		);

		expect(
			(screen.getByRole("menuitem", { name: "Delete" }) as HTMLButtonElement)
				.disabled,
		).toBe(false);
	});

	it("toggles auto-deploy from the row menu", async () => {
		const onChanged = vi.fn();
		render(
			<EnvironmentSwitcher
				state={state()}
				onCreateEnvironment={() => {}}
				onChanged={onChanged}
				onNavigateEnvironment={() => {}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Environment" }));
		fireEvent.click(
			screen.getByRole("button", { name: /environment actions for staging/i }),
		);
		fireEvent.click(
			screen.getByRole("menuitemcheckbox", { name: "Turn auto-deploy off" }),
		);

		await waitFor(() =>
			expect(doUpdateEnvironmentAutoDeployMock).toHaveBeenCalledWith({
				data: { environmentId: "environment-2", autoDeploy: false },
			}),
		);
		expect(onChanged).toHaveBeenCalled();
	});

	it("marks environments with auto-deploy off as manual", () => {
		render(
			<EnvironmentSwitcher
				state={state()}
				onCreateEnvironment={() => {}}
				onChanged={() => {}}
				onNavigateEnvironment={() => {}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Environment" }));
		expect(screen.getAllByText("manual").length).toBeGreaterThan(0);
	});
});

function state(): DashboardHomeState {
	const production = {
		id: "environment-1",
		projectId: "project-1",
		name: "production",
		kind: "persistent" as const,
		isProduction: true,
		autoDeploy: false,
	};
	const staging = {
		id: "environment-2",
		projectId: "project-1",
		name: "staging",
		kind: "persistent" as const,
		isProduction: false,
		autoDeploy: true,
	};
	return {
		user: { id: "user-1", email: "user@example.com" },
		project: {
			id: "project-1",
			name: "test-project",
			kind: "PROJECT_KIND_USER",
		},
		projects: [],

		environments: [production, staging],
		environment: production,
		onboarding: {
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "",
			repositorySelector: "",
			trackedRef: "main",
			builder: "BUILDER_KIND_DOCKERFILE",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		services: [],
		servicesRevision: 0,
		selectedServiceId: null,
		publicBaseURL: "https://dashboard.example.test",
		ingressTargetHost: "platform.example.test",
		domainBindings: [],
		controlPlaneReachable: true,
	};
}
