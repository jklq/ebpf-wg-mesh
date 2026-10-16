import { createFileRoute } from "@tanstack/react-router";
import { BillingPage } from "#/features/billing/billing-page";
import { WorkspacePageSkeleton } from "#/features/workspace/workspace-skeleton";
import { loadWorkspaceBilling } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

export const Route = createFileRoute("/_workspace/workspace/billing/")({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: ({ deps }) =>
		loadWorkspaceBilling({ data: { workspaceId: deps.workspace } }),
	pendingComponent: () => <WorkspacePageSkeleton section="billing" />,
	component: BillingRoute,
});

function BillingRoute() {
	const { shell, billing, statements, liveProjectIds } = Route.useLoaderData();
	return (
		<BillingPage
			key={shell.workspace.id}
			shell={shell}
			billing={billing}
			statements={statements}
			liveProjectIds={liveProjectIds}
		/>
	);
}
