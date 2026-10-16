import {
	createFileRoute,
	Outlet,
	useRouterState,
} from "@tanstack/react-router";
import { useEffect, useState } from "react";
import {
	type WorkspaceSection,
	WorkspaceShell,
} from "#/features/workspace/workspace-shell";
import {
	WorkspacePageSkeleton,
	WorkspaceShellSkeleton,
} from "#/features/workspace/workspace-skeleton";
import type { DashboardWorkspaceShell } from "#/lib/dashboard/core/types.server";

export const Route = createFileRoute("/_workspace")({
	component: WorkspaceLayout,
});

function WorkspaceLayout() {
	const { shell, location, navigating, failed } = useRouterState({
		select: (state) => {
			const match = state.matches.find(
				(entry) =>
					entry.routeId === "/_workspace/dashboard" ||
					entry.routeId === "/_workspace/workspace/people" ||
					entry.routeId === "/_workspace/workspace/settings" ||
					entry.routeId === "/_workspace/workspace/billing/" ||
					entry.routeId ===
						"/_workspace/workspace/billing/statements/$statementId" ||
					entry.routeId === "/_workspace/workspace/billing/checkout/success" ||
					entry.routeId === "/_workspace/workspace/billing/checkout/cancel",
			);
			return {
				shell: match?.loaderData?.shell,
				failed: match?.status === "error" || match?.status === "notFound",
				location: state.location,
				navigating:
					match?.status === "pending" ||
					(state.isLoading &&
						state.location.href !== state.resolvedLocation?.href),
			};
		},
	});
	// Keep the sidebar available while a different child route loads.
	const [previousShell, setPreviousShell] = useState<DashboardWorkspaceShell>();
	useEffect(() => {
		if (shell) setPreviousShell(shell);
	}, [shell]);
	const displayedShell = shell ?? previousShell;
	const section: WorkspaceSection =
		location.pathname === "/workspace/people"
			? "people"
			: location.pathname === "/workspace/settings"
				? "settings"
				: location.pathname.startsWith("/workspace/billing")
					? "billing"
					: "projects";
	if (!displayedShell)
		return failed ? <Outlet /> : <WorkspaceShellSkeleton section={section} />;
	const workspace = location.search.workspace
		? displayedShell.workspaces.find(
				(entry) => entry.id === location.search.workspace,
			)
		: undefined;
	const optimisticShell = workspace
		? { ...displayedShell, workspace }
		: displayedShell;
	return (
		<WorkspaceShell shell={optimisticShell} section={section}>
			{navigating ? <WorkspacePageSkeleton section={section} /> : <Outlet />}
		</WorkspaceShell>
	);
}
