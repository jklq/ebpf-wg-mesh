import { createFileRoute, redirect } from "@tanstack/react-router";

import {
	DashboardCanvasSkeleton,
	DashboardPage,
} from "./-dashboard/dashboard-page";
import { loadHome } from "./-dashboard/server-fns";

export const Route = createFileRoute("/")({
	loader: async () => {
		const state = await loadHome();
		if (state.environment) {
			throw redirect({
				to: "/environments/$environmentId",
				params: { environmentId: state.environment.id },
			});
		}
		return state;
	},
	pendingComponent: DashboardPendingRoute,
	component: DashboardRoute,
});

function DashboardRoute() {
	const state = Route.useLoaderData();
	return <DashboardPage state={state} />;
}

function DashboardPendingRoute() {
	return <DashboardCanvasSkeleton showTopbar />;
}
