import { createFileRoute } from "@tanstack/react-router";
import { ProjectSettingsPage } from "#/features/project-settings/project-settings-page";
import { loadProjectSettings } from "#/lib/dashboard/server-functions";
export const Route = createFileRoute("/projects/$projectId/settings")({
	loader: async ({ params }) => ({
		...(await loadProjectSettings({ data: { projectId: params.projectId } })),
		nowMs: Date.now(),
	}),
	component: ProjectSettingsRoute,
});
function ProjectSettingsRoute() {
	return <ProjectSettingsPage settings={Route.useLoaderData()} />;
}
