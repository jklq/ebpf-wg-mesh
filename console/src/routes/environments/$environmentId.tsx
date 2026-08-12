import { createFileRoute, redirect } from "@tanstack/react-router";

import {
	DashboardCanvasSkeleton,
	DashboardPage,
} from "../-dashboard/dashboard-page";
import { loadHome } from "../-dashboard/server-fns";

export const Route = createFileRoute("/environments/$environmentId")({
	loader: async ({ params }) => {
		const state = await loadHome({
			data: { environmentId: params.environmentId },
		});
		if (state.environment && state.environment.id !== params.environmentId) {
			throw redirect({
				to: "/environments/$environmentId",
				params: { environmentId: state.environment.id },
			});
		}
		return state;
	},
	pendingComponent: DashboardPendingRoute,
	component: DashboardEnvironmentRoute,
});

function DashboardEnvironmentRoute() {
	return <DashboardPage state={Route.useLoaderData()} />;
}

function DashboardPendingRoute() {
	return <DashboardCanvasSkeleton showTopbar />;
}
