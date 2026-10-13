import { createFileRoute } from "@tanstack/react-router";
import { PeoplePage } from "#/features/workspace/people-page";
import { loadWorkspacePeople } from "#/lib/dashboard/server-functions";
import { validateWorkspaceSearch } from "#/routes/-workspace-search";

export const Route = createFileRoute("/workspace/people")({
	validateSearch: validateWorkspaceSearch,
	loaderDeps: ({ search }) => ({ workspace: search.workspace }),
	loader: async ({ deps }) => ({
		...(await loadWorkspacePeople({ data: { workspaceId: deps.workspace } })),
		nowMs: Date.now(),
	}),
	component: PeopleRoute,
});

function PeopleRoute() {
	const { shell, members, invitations, nowMs } = Route.useLoaderData();
	return (
		<PeoplePage
			shell={shell}
			members={members}
			invitations={invitations}
			nowMs={nowMs}
		/>
	);
}
