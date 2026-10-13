import type {
	DashboardWorkspace,
	DashboardWorkspaceRole,
} from "#/lib/dashboard/core/types.server";

export const workspaceRoles: Array<{
	role: DashboardWorkspaceRole;
	label: string;
	description: string;
}> = [
	{
		role: "WORKSPACE_ROLE_ADMIN",
		label: "Admin",
		description: "Manages the workspace, its people, and every project.",
	},
	{
		role: "WORKSPACE_ROLE_MEMBER",
		label: "Member",
		description: "Creates, deploys, and changes projects.",
	},
	{
		role: "WORKSPACE_ROLE_VIEWER",
		label: "Viewer",
		description: "Sees projects, deployments, and logs.",
	},
];

export function roleLabel(role: string | undefined): string {
	return workspaceRoles.find((entry) => entry.role === role)?.label ?? "None";
}

export function canManageWorkspace(workspace: DashboardWorkspace): boolean {
	return workspace.role === "WORKSPACE_ROLE_ADMIN";
}

export function canEditProjects(workspace: DashboardWorkspace): boolean {
	return (
		workspace.role === "WORKSPACE_ROLE_ADMIN" ||
		workspace.role === "WORKSPACE_ROLE_MEMBER"
	);
}
