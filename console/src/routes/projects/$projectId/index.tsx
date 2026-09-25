import { createFileRoute, redirect } from "@tanstack/react-router";

import { DashboardCanvasSkeleton } from "../../-dashboard/dashboard-page";
import { resolveProjectLanding } from "../../-dashboard/server-fns";

// Opens a project on its production environment. A project without live
// environments opens on its settings.
export const Route = createFileRoute("/projects/$projectId/")({
	loader: async ({ params }) => {
		const environmentId = await resolveProjectLanding({
			data: { projectId: params.projectId },
		});
		if (environmentId) {
			throw redirect({
				to: "/environments/$environmentId",
				params: { environmentId },
			});
		}
		throw redirect({
			to: "/projects/$projectId/settings",
			params: { projectId: params.projectId },
		});
	},
	pendingComponent: () => <DashboardCanvasSkeleton showTopbar />,
});
