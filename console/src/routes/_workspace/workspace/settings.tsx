import { createFileRoute } from "@tanstack/react-router";
import { WorkspaceSettingsPage } from "#/features/workspace/workspace-settings-page";
import { WorkspacePageSkeleton } from "#/features/workspace/workspace-skeleton";
import { loadWorkspaceShell } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

export const Route = createFileRoute("/_workspace/workspace/settings")({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: async ({ deps }) => ({
		shell: await loadWorkspaceShell({ data: { workspaceId: deps.workspace } }),
	}),
	pendingComponent: () => <WorkspacePageSkeleton section="settings" />,
	component: WorkspaceSettingsRoute,
});

function WorkspaceSettingsRoute() {
	const { shell } = Route.useLoaderData();
	return <WorkspaceSettingsPage key={shell.workspace.id} shell={shell} />;
}
