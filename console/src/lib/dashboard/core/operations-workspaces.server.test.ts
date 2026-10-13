import { describe, expect, it } from "vitest";
import * as auth from "#/lib/dashboard/core/auth.server";
import * as workspaces from "#/lib/dashboard/core/operations-workspaces.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";
import { jsonFixture, serviceFixture } from "#/lib/dashboard/testkit/protocol";
import {
	EnvironmentSchema,
	ProjectSchema,
	WorkspaceSchema,
} from "#/lib/platform-gen/platform_pb";

async function signedInHarness() {
	const harness = createDashboardTestHarness();
	await auth.completeAuthCallback(harness.runtime, {
		userId: "user-1",
		email: "user@example.com",
		redirectTo: "/",
	});
	harness.platform.workspaces.push(
		jsonFixture(WorkspaceSchema, {
			id: "workspace-acme",
			name: "Acme",
			role: "WORKSPACE_ROLE_MEMBER",
		}),
	);
	return harness;
}

describe("workspace operations", () => {
	it("loads overview cards with one project request and no per-project requests", async () => {
		const harness = await signedInHarness();
		harness.platform.projects = [
			jsonFixture(ProjectSchema, {
				id: "project-1",
				workspaceId: "workspace-acme",
				name: "web",
				createdAt: "2026-10-01T00:00:00Z",
			}),
			jsonFixture(ProjectSchema, {
				id: "project-2",
				workspaceId: "workspace-acme",
				name: "empty",
				createdAt: "2026-10-02T00:00:00Z",
			}),
		];
		harness.platform.environments = [
			jsonFixture(EnvironmentSchema, {
				id: "staging",
				projectId: "project-1",
				name: "Staging",
			}),
			jsonFixture(EnvironmentSchema, {
				id: "production",
				projectId: "project-1",
				name: "Production",
				isProduction: true,
			}),
		];
		harness.platform.services = [
			serviceFixture({
				id: "online",
				environmentId: "production",
				name: "web",
				readyReplicaCount: 1,
			}),
			serviceFixture({
				id: "offline",
				environmentId: "production",
				name: "worker",
			}),
			serviceFixture({ id: "staging-web", environmentId: "staging" }),
		];

		const overview = await workspaces.loadWorkspaceOverview(
			harness.runtime,
			"workspace-acme",
		);

		expect(overview.projects.map((entry) => entry.project.id)).toEqual([
			"project-2",
			"project-1",
		]);
		expect(overview.projects[0]).toMatchObject({
			environmentCount: 0,
			services: [],
		});
		expect(overview.projects[0].landingEnvironment).toBeUndefined();
		expect(overview.projects[1]).toMatchObject({
			environmentCount: 2,
			landingEnvironment: { id: "production", name: "Production" },
			services: [
				{ id: "online", name: "web", online: true },
				{ id: "offline", name: "worker", online: false },
			],
		});
		expect(harness.platform.calls.listProjects).toMatchObject([
			{ workspaceId: "workspace-acme", includeSummaries: true },
		]);
		expect(harness.platform.calls.listEnvironments).toEqual([]);
		expect(harness.platform.calls.listServices).toEqual([]);
	});

	it("shows the requested workspace, then the remembered one, then the personal one", async () => {
		const harness = await signedInHarness();

		const requested = await workspaces.loadWorkspaceShell(
			harness.runtime,
			"workspace-acme",
		);
		expect(requested.workspace.id).toBe("workspace-acme");

		const remembered = await workspaces.loadWorkspaceShell(harness.runtime);
		expect(remembered.workspace.id).toBe("workspace-acme");

		const foreign = await workspaces.loadWorkspaceShell(
			harness.runtime,
			"workspace-someone-else",
		);
		expect(foreign.workspace.id).toBe("workspace-acme");

		harness.platform.workspaces = harness.platform.workspaces.filter(
			(entry) => entry.id !== "workspace-acme",
		);
		const fallback = await workspaces.loadWorkspaceShell(harness.runtime);
		expect(fallback.workspace).toMatchObject({
			id: "workspace-personal",
			personal: true,
		});
	});

	it("creates generated-name projects in the workspace and opens production", async () => {
		const harness = await signedInHarness();

		const created = await workspaces.createWorkspaceProjectFromSession(
			harness.runtime,
			{ workspaceId: "workspace-acme" },
		);

		expect(harness.platform.calls.createProject).toMatchObject([
			{ workspaceId: "workspace-acme", name: expect.stringMatching(/\S/) },
		]);
		expect(created).toEqual({
			projectId: "project-1",
			environmentId: "environment-1",
		});
		const overview = await workspaces.loadWorkspaceOverview(
			harness.runtime,
			"workspace-acme",
		);
		expect(overview.projects.map((entry) => entry.project.id)).toEqual([
			"project-1",
		]);
		const personal = await workspaces.loadWorkspaceOverview(
			harness.runtime,
			"workspace-personal",
		);
		expect(personal.projects).toEqual([]);
	});

	it("labels members with their console profiles", async () => {
		const harness = await signedInHarness();
		harness.platform.workspaceMembers = [
			{ userId: "user-1", role: "WORKSPACE_ROLE_ADMIN" },
			{ userId: "unknown-user", role: "WORKSPACE_ROLE_VIEWER" },
		];

		const people = await workspaces.loadWorkspacePeople(
			harness.runtime,
			"workspace-acme",
		);

		expect(people.members).toEqual([
			{
				userId: "user-1",
				role: "WORKSPACE_ROLE_ADMIN",
				email: "user@example.com",
				githubLogin: undefined,
			},
			{ userId: "unknown-user", role: "WORKSPACE_ROLE_VIEWER" },
		]);
	});
});
