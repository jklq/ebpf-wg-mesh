import { createFileRoute } from "@tanstack/react-router";
import { Server } from "lucide-react";
import { PageSkeleton } from "#/components/layout/page-skeleton";
import { FleetPage } from "#/features/fleet/fleet-page";
import { loadFleet } from "#/features/fleet/server/functions";
export const Route = createFileRoute("/fleet")({
	loader: () => loadFleet(),
	pendingComponent: () => (
		<PageSkeleton title="Agent fleet" icon={<Server size={15} />} />
	),
	component: FleetRoute,
});
function FleetRoute() {
	return <FleetPage initialFleet={Route.useLoaderData()} />;
}
