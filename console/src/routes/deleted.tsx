import { createFileRoute } from "@tanstack/react-router";
import { RecentlyDeletedPage } from "#/features/deleted/deleted-page";
import { loadRecentlyDeleted } from "#/lib/dashboard/server-functions";
export const Route = createFileRoute("/deleted")({
	loader: async () => ({
		resources: await loadRecentlyDeleted(),
		nowMs: Date.now(),
	}),
	component: RecentlyDeletedRoute,
});
function RecentlyDeletedRoute() {
	return <RecentlyDeletedPage state={Route.useLoaderData()} />;
}
