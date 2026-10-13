/** `?workspace=<id>` selects the workspace a workspace page shows. */
export function validateWorkspaceSearch(search: Record<string, unknown>): {
	workspace?: string;
} {
	return {
		workspace:
			typeof search.workspace === "string" && search.workspace.length > 0
				? search.workspace
				: undefined,
	};
}
