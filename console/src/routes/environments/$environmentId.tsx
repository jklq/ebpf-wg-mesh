import { createFileRoute, redirect } from "@tanstack/react-router";

import {
	DashboardCanvasSkeleton,
	DashboardPage,
} from "../-dashboard/dashboard-page";
import { loadHome } from "../-dashboard/server-fns";

export const Route = createFileRoute("/environments/$environmentId")({
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
