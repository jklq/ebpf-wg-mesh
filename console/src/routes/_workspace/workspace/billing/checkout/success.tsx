import { createFileRoute } from "@tanstack/react-router";
import { CheckoutReturnPage } from "#/features/billing/checkout-return-page";
import { WorkspacePageSkeleton } from "#/features/workspace/workspace-skeleton";
import { loadWorkspaceBilling } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

/** CONTROLPLANE_BILLING_CHECKOUT_SUCCESS_URL points here. */
export const Route = createFileRoute(
	"/_workspace/workspace/billing/checkout/success",
)({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: ({ deps }) =>
		loadWorkspaceBilling({ data: { workspaceId: deps.workspace } }),
	pendingComponent: () => <WorkspacePageSkeleton section="billing" />,
	component: CheckoutRoute,
});

function CheckoutRoute() {
	const { shell, billing } = Route.useLoaderData();
	return (
		<CheckoutReturnPage
			key={shell.workspace.id}
			outcome="success"
			shell={shell}
			billing={billing}
		/>
	);
}
