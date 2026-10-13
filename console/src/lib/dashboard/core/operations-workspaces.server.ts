import {
	requireSession,
	sessionCookieOptions,
} from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	nextGeneratedProjectName,
	optionalPlatformResult,
	storeCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardProject,
	type DashboardProjectSummary,
	DashboardValidationError,
	type DashboardWorkspace,
	type DashboardWorkspaceInvitation,
	type DashboardWorkspaceMember,
	type DashboardWorkspaceRole,
	type DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import { OpsService, PlatformService } from "#/lib/platform-gen/platform_pb";

/** Remembers the workspace a user last viewed, per browser. */
const workspaceCookieName = "dashboard_workspace";
const workspaceCookieMaxAgeMs = 365 * 24 * 60 * 60 * 1000;

function rememberWorkspace(runtime: DashboardRuntime, workspaceId: string) {
	runtime.cookies.set(
		workspaceCookieName,
		workspaceId,
		sessionCookieOptions(
			runtime.config,
			new Date(runtime.now().getTime() + workspaceCookieMaxAgeMs),
		),
	);
}

/**
 * Resolves the workspace being viewed: the requested one, else the last one
 * viewed, else the personal workspace. Requests for workspaces the user is not
 * in fall back the same way.
 */
export async function loadWorkspaceShell(
	runtime: DashboardRuntime,
	requestedWorkspaceId?: string,
): Promise<DashboardWorkspaceShell> {
	const session = await requireSession(runtime);
	const [listing, fleet, githubAccount] = await Promise.all([
		runtime.platform.call(
			PlatformService.method.listWorkspaces,
			session.user,
			{},
		),
		optionalPlatformResult(
			runtime.platform.call(OpsService.method.listFleet, session.user, {}),
		),
		storeCall(runtime, "getGitHubAccount", (store) =>
			store.getGitHubAccount(session.user.id),
		),
	]);
	const workspaces = listing.workspaces ?? [];
	const remembered = runtime.cookies.get(workspaceCookieName);
	const workspace =
		workspaces.find((entry) => entry.id === requestedWorkspaceId) ??
		workspaces.find((entry) => entry.id === remembered) ??
		workspaces[0];
	if (!workspace) {
		throw new Error("the control plane returned no personal workspace");
	}
	if (workspace.id !== remembered) rememberWorkspace(runtime, workspace.id);
	return {
		user: session.user,
		githubLogin: githubAccount?.login,
		workspace,
		workspaces,
		invitations: listing.invitations ?? [],
		canManageFleet: fleet !== undefined,
	};
}

export async function loadWorkspaceOverview(
	runtime: DashboardRuntime,
	requestedWorkspaceId?: string,
): Promise<{
	shell: DashboardWorkspaceShell;
	projects: Array<DashboardProjectSummary>;
}> {
	const shell = await loadWorkspaceShell(runtime, requestedWorkspaceId);
	const projects = await runtime.platform
		.call(PlatformService.method.listProjects, shell.user, {
			workspaceId: shell.workspace.id,
		})
		.then((response) => response.projects ?? []);
	const summaries = await Promise.all(
		projects.map((project) => summarizeProject(runtime, shell, project)),
	);
	return {
		shell,
		projects: summaries.sort((a, b) =>
			(b.project.createdAt ?? "").localeCompare(a.project.createdAt ?? ""),
		),
	};
}

async function summarizeProject(
	runtime: DashboardRuntime,
	shell: DashboardWorkspaceShell,
	project: DashboardProject,
): Promise<DashboardProjectSummary> {
	const environments = await runtime.platform
		.call(PlatformService.method.listEnvironments, shell.user, {
			projectId: project.id,
		})
		.then((response) => response.environments ?? []);
	const landing =
		environments.find((entry) => entry.isProduction) ?? environments[0];
	const services = landing
		? await runtime.platform
				.call(PlatformService.method.listServices, shell.user, {
					environmentId: landing.id,
				})
				.then((response) => response.services ?? [])
		: [];
	return {
		project,
		environmentCount: environments.length,
		landingEnvironment: landing
			? { id: landing.id, name: landing.name }
			: undefined,
		services: services.map((service) => ({
			id: service.id,
			name: service.name,
			online: (service.readyReplicaCount ?? 0) > 0,
		})),
	};
}

/** Creates a project and returns the environment it opens on. */
export async function createWorkspaceProjectFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; name?: string },
): Promise<{ projectId: string; environmentId: string }> {
	const session = await requireSession(runtime);
	let name = input.name?.trim() ?? "";
	if (name === "") {
		const existing = await runtime.platform
			.call(PlatformService.method.listProjects, session.user, {
				workspaceId: input.workspaceId,
				includeDeleted: true,
			})
			.then((response) => response.projects ?? []);
		name = nextGeneratedProjectName(existing, runtime.randomUUID());
	}
	const project = await runtime.platform.call(
		PlatformService.method.createProject,
		session.user,
		{ workspaceId: input.workspaceId, name },
	);
	const environments = await runtime.platform
		.call(PlatformService.method.listEnvironments, session.user, {
			projectId: project.id,
		})
		.then((response) => response.environments ?? []);
	const environment =
		environments.find((entry) => entry.isProduction) ?? environments[0];
	if (!environment) {
		throw new DashboardValidationError({
			message: "Project has no production environment.",
		});
	}
	return { projectId: project.id, environmentId: environment.id };
}

export async function createWorkspaceFromSession(
	runtime: DashboardRuntime,
	name: string,
): Promise<DashboardWorkspace> {
	const session = await requireSession(runtime);
	const workspace = await runtime.platform.call(
		PlatformService.method.createWorkspace,
		session.user,
		{ name: name.trim() },
	);
	rememberWorkspace(runtime, workspace.id);
	return workspace;
}

export async function renameWorkspaceFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; name: string },
): Promise<DashboardWorkspace> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.renameWorkspace,
		session.user,
		{ workspaceId: input.workspaceId, name: input.name.trim() },
	);
}

export async function deleteWorkspaceFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; confirmationName: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await runtime.platform.call(
		PlatformService.method.deleteWorkspace,
		session.user,
		input,
	);
}

export async function loadWorkspacePeople(
	runtime: DashboardRuntime,
	requestedWorkspaceId?: string,
): Promise<{
	shell: DashboardWorkspaceShell;
	members: Array<DashboardWorkspaceMember>;
	invitations: Array<DashboardWorkspaceInvitation>;
}> {
	const shell = await loadWorkspaceShell(runtime, requestedWorkspaceId);
	const listing = await runtime.platform.call(
		PlatformService.method.listWorkspaceMembers,
		shell.user,
		{ workspaceId: shell.workspace.id },
	);
	const members = listing.members ?? [];
	const profiles = await storeCall(runtime, "listUserProfiles", (store) =>
		store.listUserProfiles(members.map((member) => member.userId)),
	);
	const profileByID = new Map(profiles.map((entry) => [entry.id, entry]));
	return {
		shell,
		members: members.flatMap((member) => {
			const role = knownRole(member.role);
			if (!role) return [];
			const profile = profileByID.get(member.userId);
			return [
				{
					userId: member.userId,
					role,
					joinedAt: member.joinedAt,
					email: profile?.email,
					githubLogin: profile?.githubLogin,
				},
			];
		}),
		invitations: listing.invitations ?? [],
	};
}

function knownRole(
	role: string | undefined,
): DashboardWorkspaceRole | undefined {
	switch (role) {
		case "WORKSPACE_ROLE_ADMIN":
		case "WORKSPACE_ROLE_MEMBER":
		case "WORKSPACE_ROLE_VIEWER":
			return role;
		default:
			return undefined;
	}
}

export async function inviteWorkspaceMemberFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; email: string; role: DashboardWorkspaceRole },
): Promise<DashboardWorkspaceInvitation> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.inviteWorkspaceMember,
		session.user,
		{ ...input, email: input.email.trim() },
	);
}

export async function revokeWorkspaceInvitationFromSession(
	runtime: DashboardRuntime,
	invitationId: string,
): Promise<void> {
	const session = await requireSession(runtime);
	await runtime.platform.call(
		PlatformService.method.revokeWorkspaceInvitation,
		session.user,
		{ invitationId },
	);
}

export async function answerWorkspaceInvitationFromSession(
	runtime: DashboardRuntime,
	input: { invitationId: string; accept: boolean },
): Promise<void> {
	const session = await requireSession(runtime);
	if (!input.accept) {
		await runtime.platform.call(
			PlatformService.method.declineWorkspaceInvitation,
			session.user,
			{ invitationId: input.invitationId },
		);
		return;
	}
	const workspace = await runtime.platform.call(
		PlatformService.method.acceptWorkspaceInvitation,
		session.user,
		{ invitationId: input.invitationId },
	);
	rememberWorkspace(runtime, workspace.id);
}

export async function updateWorkspaceMemberRoleFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; userId: string; role: DashboardWorkspaceRole },
): Promise<void> {
	const session = await requireSession(runtime);
	await runtime.platform.call(
		PlatformService.method.updateWorkspaceMemberRole,
		session.user,
		input,
	);
}

/** Removes a member; removing yourself leaves the workspace. */
export async function removeWorkspaceMemberFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; userId: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await runtime.platform.call(
		PlatformService.method.removeWorkspaceMember,
		session.user,
		input,
	);
}

export async function moveProjectFromSession(
	runtime: DashboardRuntime,
	input: { projectId: string; workspaceId: string },
): Promise<DashboardProject> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.moveProject,
		session.user,
		input,
	);
}
