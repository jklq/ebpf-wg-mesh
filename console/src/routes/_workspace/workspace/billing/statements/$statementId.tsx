import { createFileRoute } from "@tanstack/react-router";
import { StatementPage } from "#/features/billing/statements";
import { WorkspacePageSkeleton } from "#/features/workspace/workspace-skeleton";
import { loadBillingStatement } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

export const Route = createFileRoute(
	"/_workspace/workspace/billing/statements/$statementId",
)({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: ({ deps, params }) =>
		loadBillingStatement({
			data: { workspaceId: deps.workspace, statementId: params.statementId },
		}),
	pendingComponent: () => <WorkspacePageSkeleton section="billing" />,
	component: StatementRoute,
});

function StatementRoute() {
	const { shell, statement } = Route.useLoaderData();
	return (
		<StatementPage key={statement.id} shell={shell} statement={statement} />
	);
}
