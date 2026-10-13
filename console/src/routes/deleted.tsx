import { createFileRoute } from "@tanstack/react-router";
import { History } from "lucide-react";
import { PageSkeleton } from "#/components/layout/page-skeleton";
import { RecentlyDeletedPage } from "#/features/deleted/deleted-page";
import { loadRecentlyDeleted } from "#/lib/dashboard/server-functions";
export const Route = createFileRoute("/deleted")({
	loader: async () => ({
		resources: await loadRecentlyDeleted(),
		nowMs: Date.now(),
	}),
	pendingComponent: () => (
		<PageSkeleton title="Recently deleted" icon={<History size={15} />} />
	),
	component: RecentlyDeletedRoute,
});
function RecentlyDeletedRoute() {
	return <RecentlyDeletedPage state={Route.useLoaderData()} />;
}
