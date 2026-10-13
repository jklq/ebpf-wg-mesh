import { createFileRoute } from "@tanstack/react-router";
import { ProjectsPage } from "#/features/workspace/projects-page";
import { loadWorkspaceOverview } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

export const Route = createFileRoute("/dashboard")({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: ({ deps }) =>
		loadWorkspaceOverview({ data: { workspaceId: deps.workspace } }),
	component: DashboardRoute,
});

function DashboardRoute() {
	const { shell, projects } = Route.useLoaderData();
	return <ProjectsPage shell={shell} projects={projects} />;
}
