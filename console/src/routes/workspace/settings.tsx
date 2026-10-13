import { createFileRoute } from "@tanstack/react-router";
import { WorkspaceSettingsPage } from "#/features/workspace/workspace-settings-page";
import { loadWorkspaceShell } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

export const Route = createFileRoute("/workspace/settings")({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: ({ deps }) =>
		loadWorkspaceShell({ data: { workspaceId: deps.workspace } }),
	component: WorkspaceSettingsRoute,
});

function WorkspaceSettingsRoute() {
	const shell = Route.useLoaderData();
	return <WorkspaceSettingsPage key={shell.workspace.id} shell={shell} />;
}
