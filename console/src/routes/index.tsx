import { createFileRoute, redirect } from "@tanstack/react-router";

import {
	DashboardCanvasSkeleton,
	DashboardPage,
} from "./-dashboard/dashboard-page";
import { loadHome } from "./-dashboard/server-fns";

export const Route = createFileRoute("/")({
	validateSearch: (
		search: Record<string, unknown>,
	): {
		serviceId?: string;
	} => ({
		serviceId:
			typeof search.serviceId === "string" && search.serviceId.length > 0
				? search.serviceId
				: undefined,
	}),
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
	const search = Route.useSearch();
	const urlSelectedServiceId =
		search.serviceId &&
		state.services.some((service) => service.id === search.serviceId)
			? search.serviceId
			: null;
	return (
		<DashboardPage state={state} urlSelectedServiceId={urlSelectedServiceId} />
	);
}

function DashboardPendingRoute() {
	return <DashboardCanvasSkeleton showTopbar />;
}
