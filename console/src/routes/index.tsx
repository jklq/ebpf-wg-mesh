import { createFileRoute, redirect } from "@tanstack/react-router";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

import {
	DashboardCanvasSkeleton,
	DashboardPage,
} from "./-dashboard/dashboard-page";
import { loadHome } from "./-dashboard/server-fns";

export async function loadHomeRouteState(service: {
	loadDashboardHome(): Promise<DashboardHomeState | null>;
}): Promise<DashboardHomeState> {
	const state = await service.loadDashboardHome();
	if (!state) {
		throw redirect({
			to: "/login",
			search: { redirect: undefined, error: undefined },
		});
	}
	return state;
}

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
