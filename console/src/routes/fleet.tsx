import { createFileRoute } from "@tanstack/react-router";
import { FleetPage } from "#/features/fleet/fleet-page";
import { loadFleet } from "#/features/fleet/server/functions";
export const Route = createFileRoute("/fleet")({
	loader: () => loadFleet(),
	component: FleetRoute,
});
function FleetRoute() {
	return <FleetPage initialFleet={Route.useLoaderData()} />;
}
