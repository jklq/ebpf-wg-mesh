import { createFileRoute } from "@tanstack/react-router";
import { PeoplePage } from "#/features/workspace/people-page";
import { WorkspacePageSkeleton } from "#/features/workspace/workspace-skeleton";
import { loadWorkspacePeople } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

export const Route = createFileRoute("/_workspace/workspace/people")({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: async ({ deps }) => ({
		...(await loadWorkspacePeople({ data: { workspaceId: deps.workspace } })),
		nowMs: Date.now(),
	}),
	pendingComponent: () => <WorkspacePageSkeleton section="people" />,
	component: PeopleRoute,
});

function PeopleRoute() {
	const { shell, members, invitations, nowMs } = Route.useLoaderData();
	return (
		<PeoplePage
			key={shell.workspace.id}
			shell={shell}
			members={members}
			invitations={invitations}
			nowMs={nowMs}
		/>
	);
}
