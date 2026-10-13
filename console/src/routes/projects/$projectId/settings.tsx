import { createFileRoute } from "@tanstack/react-router";
import { Settings } from "lucide-react";
import { PageSkeleton } from "#/components/layout/page-skeleton";
import { ProjectSettingsPage } from "#/features/project-settings/project-settings-page";
import { loadProjectSettings } from "#/lib/dashboard/server-functions";
export const Route = createFileRoute("/projects/$projectId/settings")({
	loader: async ({ params }) => ({
		...(await loadProjectSettings({ data: { projectId: params.projectId } })),
		nowMs: Date.now(),
	}),
	pendingComponent: () => (
		<PageSkeleton title="Project settings" icon={<Settings size={15} />} />
	),
	component: ProjectSettingsRoute,
});
function ProjectSettingsRoute() {
	return <ProjectSettingsPage settings={Route.useLoaderData()} />;
}
