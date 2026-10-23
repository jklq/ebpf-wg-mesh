// @vitest-environment jsdom

import {
	createMemoryHistory,
	createRootRoute,
	createRouter,
	Outlet,
	RouterProvider,
} from "@tanstack/react-router";
import {
	act,
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DashboardWorkspaceShell } from "#/lib/dashboard/core/types.server";
import { Route as workspaceRoute } from "#/routes/_workspace";
import { Route as projectsRoute } from "#/routes/_workspace/dashboard";
import { Route as peopleRoute } from "#/routes/_workspace/workspace/people";
import { Route as settingsRoute } from "#/routes/_workspace/workspace/settings";

const { overview, people, shellLoader } = vi.hoisted(() => ({
	overview: vi.fn(),
	people: vi.fn(),
	shellLoader: vi.fn(),
}));
vi.mock("#/lib/dashboard/server-functions", () => ({
	loadWorkspaceOverview: overview,
	loadWorkspacePeople: people,
	loadWorkspaceShell: shellLoader,
	doCreateWorkspaceProject: vi.fn(),
	doCreateWorkspace: vi.fn(),
	doAnswerWorkspaceInvitation: vi.fn(),
	doRemoveWorkspaceMember: vi.fn(),
	doRevokeWorkspaceInvitation: vi.fn(),
	doUpdateWorkspaceMemberRole: vi.fn(),
	doInviteWorkspaceMember: vi.fn(),
	doRenameWorkspace: vi.fn(),
	doDeleteWorkspace: vi.fn(),
}));

const workspaces: DashboardWorkspaceShell["workspaces"] = [
	{ id: "team", name: "Team", personal: false, role: "WORKSPACE_ROLE_ADMIN" },
	{
		id: "other",
		name: "Other",
		personal: false,
		role: "WORKSPACE_ROLE_MEMBER",
	},
];
function shell(id = "team"): DashboardWorkspaceShell {
	return {
		user: { id: "user", email: "user@example.test" },
		workspace: workspaces.find((entry) => entry.id === id) ?? workspaces[0],
		workspaces,
		invitations: [],
		canManageFleet: false,
	};
}
function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((done) => {
		resolve = done;
	});
	return { promise, resolve };
}

beforeEach(() => {
	vi.resetAllMocks();
	overview.mockImplementation(async ({ data }) => ({
		shell: shell(data.workspaceId),
		projects: [],
	}));
	people.mockImplementation(async ({ data }) => ({
		shell: shell(data.workspaceId),
		members: [],
		invitations: [],
	}));
	shellLoader.mockImplementation(async ({ data }) => shell(data.workspaceId));
});
afterEach(cleanup);

async function open(initial = "/dashboard?workspace=team") {
	const root = createRootRoute({ component: Outlet });
	const layout = workspaceRoute.update({
		id: "/_workspace",
		getParentRoute: () => root,
	} as never);
	const projects = projectsRoute.update({
		path: "/dashboard",
		getParentRoute: () => layout,
	} as never);
	const members = peopleRoute.update({
		path: "/workspace/people",
		getParentRoute: () => layout,
	} as never);
	const settings = settingsRoute.update({
		path: "/workspace/settings",
		getParentRoute: () => layout,
	} as never);
	const router = createRouter({
		routeTree: root.addChildren([
			layout.addChildren([projects, members, settings]),
		]),
		history: createMemoryHistory({ initialEntries: [initial] }),
		defaultPreload: "intent",
		defaultPreloadStaleTime: 30_000,
		defaultStaleTime: 10_000,
		defaultPendingMs: 0,
		defaultPendingMinMs: 0,
		defaultErrorComponent: ({ error }) => (
			<div role="alert">
				{error instanceof Error ? error.message : String(error)}
			</div>
		),
	});
	await router.load();
	render(<RouterProvider router={router} />);
	return router;
}

describe("workspace navigation", () => {
	it("switches the heading and active link immediately while keeping the sidebar mounted", async () => {
		const loading = deferred<{
			shell: DashboardWorkspaceShell;
			members: [];
			invitations: [];
		}>();
		people.mockReturnValue(loading.promise);
		await open();
		const sidebar = screen.getByRole("navigation", { name: "Workspace" });
		const link = screen.getByRole("link", { name: "People" });
		expect(link.getAttribute("href")).toBe("/workspace/people?workspace=team");
		fireEvent.click(link);
		expect(
			await screen.findByRole("status", { name: "Loading people" }),
		).toBeTruthy();
		expect(screen.getByRole("heading", { name: "People" })).toBeTruthy();
		expect(link.getAttribute("aria-current")).toBe("page");
		expect(screen.queryByText("No projects yet")).toBeNull();
		expect(screen.getByRole("navigation", { name: "Workspace" })).toBe(sidebar);
		await act(async () =>
			loading.resolve({ shell: shell(), members: [], invitations: [] }),
		);
		expect(
			await screen.findByText("0 people have access to Team"),
		).toBeTruthy();
		expect(screen.queryByRole("status")).toBeNull();
		expect(screen.getByRole("navigation", { name: "Workspace" })).toBe(sidebar);
	});

	it("reuses preloaded data and keeps the newer destination when an older request finishes", async () => {
		const router = await open();
		await act(async () => {
			await router.preloadRoute({
				to: "/workspace/settings",
				search: { workspace: "team" },
			});
		});
		expect(shellLoader).toHaveBeenCalledTimes(1);
		const loading = deferred<{
			shell: DashboardWorkspaceShell;
			members: [];
			invitations: [];
		}>();
		people.mockReturnValue(loading.promise);
		fireEvent.click(screen.getByRole("link", { name: "People" }));
		await screen.findByRole("status", { name: "Loading people" });
		fireEvent.click(screen.getByRole("link", { name: "Settings" }));
		await screen.findByText("General settings for Team");
		expect(shellLoader).toHaveBeenCalledTimes(1);
		await act(async () =>
			loading.resolve({ shell: shell(), members: [], invitations: [] }),
		);
		expect(screen.getByText("General settings for Team")).toBeTruthy();
		expect(
			screen
				.getByRole("link", { name: "Settings" })
				.getAttribute("aria-current"),
		).toBe("page");
	});

	it("shows the selected workspace immediately and uses it for the next page", async () => {
		await open();
		const loading = deferred<{
			shell: DashboardWorkspaceShell;
			projects: [];
		}>();
		overview.mockReturnValue(loading.promise);
		fireEvent.click(screen.getByRole("button", { name: "Workspace Team" }));
		fireEvent.click(screen.getByRole("menuitem", { name: /Other/ }));
		await screen.findByRole("status", { name: "Loading projects" });
		expect(
			screen.getByRole("button", { name: "Workspace Other" }),
		).toBeTruthy();
		expect(
			screen.getByRole("link", { name: "People" }).getAttribute("href"),
		).toBe("/workspace/people?workspace=other");
		await act(async () =>
			loading.resolve({ shell: shell("other"), projects: [] }),
		);
		await screen.findByText("0 projects in Other");
	});

	it("shows loader failures instead of leaving an initial skeleton stuck", async () => {
		overview.mockRejectedValue(new Error("Unable to load workspace"));
		await open();
		expect(await screen.findByRole("alert")).toBeTruthy();
		expect(screen.getByText("Unable to load workspace")).toBeTruthy();
		await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
	});
});
