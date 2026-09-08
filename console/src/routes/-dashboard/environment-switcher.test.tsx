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

const { doRenameEnvironmentMock, doDeleteEnvironmentMock } = vi.hoisted(() => ({
	doRenameEnvironmentMock: vi.fn(),
	doDeleteEnvironmentMock: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	doRenameEnvironment: doRenameEnvironmentMock,
	doDeleteEnvironment: doDeleteEnvironmentMock,
}));

beforeEach(() => {
	doRenameEnvironmentMock.mockReset().mockResolvedValue(undefined);
	doDeleteEnvironmentMock.mockReset().mockResolvedValue(undefined);
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

	it("requires typing the environment name before deleting", async () => {
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

		expect(document.activeElement).toBe(
			screen.getByLabelText("Type staging to confirm"),
		);
		const confirm = screen.getByRole("button", { name: "Delete" });
		expect((confirm as HTMLButtonElement).disabled).toBe(true);

		fireEvent.change(screen.getByLabelText("Type staging to confirm"), {
			target: { value: "staging" },
		});
		expect((confirm as HTMLButtonElement).disabled).toBe(false);
		fireEvent.click(confirm);

		await waitFor(() =>
			expect(doDeleteEnvironmentMock).toHaveBeenCalledWith({
				data: { environmentId: "environment-2" },
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

	it("does not offer deleting the production environment", () => {
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
		).toBe(true);
	});
});

function state(): DashboardHomeState {
	const production = {
		id: "environment-1",
		projectId: "project-1",
		name: "production",
		kind: "persistent" as const,
		isProduction: true,
	};
	const staging = {
		id: "environment-2",
		projectId: "project-1",
		name: "staging",
		kind: "persistent" as const,
		isProduction: false,
	};
	return {
		user: { id: "user-1", email: "user@example.com" },
		project: {
			id: "project-1",
			name: "test-project",
			kind: "PROJECT_KIND_USER",
		},
		environments: [production, staging],
		environment: production,
		onboarding: {
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "",
			repositorySelector: "",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		services: [],
		service: undefined,
		serviceStatus: undefined,
		publicBaseURL: "https://dashboard.example.test",
		ingressTargetHost: "platform.example.test",
		domainBindings: [],
		controlPlaneReachable: true,
	};
}
