package mcpserver

// preview is the uniform destructive-tool preview payload: nothing was
// executed; call again with confirm: true after the user agrees.
func preview(summary string, detail any) map[string]any {
	out := map[string]any{
		"preview":          true,
		"executed":         false,
		"requires_confirm": true,
		"summary":          summary,
		"next_step":        "Show this preview to the user. If they agree, call the same tool again with confirm: true.",
	}
	if detail != nil {
		out["detail"] = detail
	}
	return out
}

func deleted(kind, id, name string) map[string]any {
	out := map[string]any{"deleted": kind, "id": id}
	if name != "" {
		out["name"] = name
	}
	return out
}

// stagedNote explains draft semantics next to a mutating result.
func stagedNote(released bool) string {
	if released {
		return "released: the environment release was created and deployments are progressing"
	}
	return "staged: nothing is running yet; call release_environment (or repeat with release: true) to deploy"
}
