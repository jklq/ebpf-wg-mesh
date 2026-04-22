import { createFileRoute, redirect } from "@tanstack/react-router";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

import { DashboardPage } from "./-dashboard/dashboard-page";
import { NewServiceModal } from "./-dashboard/new-service-modal";
import { loadHome } from "./-dashboard/server-fns";

export { NewServiceModal };

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
	loader: async () => loadHome(),
	component: DashboardRoute,
});

function DashboardRoute() {
	const state = Route.useLoaderData();
	return <DashboardPage state={state} />;
}
