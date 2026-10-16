import { createFileRoute } from "@tanstack/react-router";
import { ChartColumn } from "lucide-react";
import { PageSkeleton } from "#/components/layout/page-skeleton";
import { usageWindow } from "#/features/usage/usage-model";
import {
	INITIAL_USAGE_RANGE,
	ProjectUsagePage,
} from "#/features/usage/usage-page";
import {
	loadProject,
	loadProjectUsage,
} from "#/lib/dashboard/server-functions";

export const Route = createFileRoute("/projects/$projectId/usage")({
	loader: async ({ params }) => {
		const [project, usage] = await Promise.all([
			loadProject({ data: { projectId: params.projectId } }),
			loadProjectUsage({
				data: {
					projectId: params.projectId,
					...usageWindow(INITIAL_USAGE_RANGE, Date.now()),
				},
			}),
		]);
		return { project, initial: { usage, loadedAtMs: Date.now() } };
	},
	pendingComponent: () => (
		<PageSkeleton title="Usage" icon={<ChartColumn size={15} />} />
	),
	component: ProjectUsageRoute,
});

function ProjectUsageRoute() {
	const { project, initial } = Route.useLoaderData();
	return (
		<ProjectUsagePage key={project.id} project={project} initial={initial} />
	);
}
