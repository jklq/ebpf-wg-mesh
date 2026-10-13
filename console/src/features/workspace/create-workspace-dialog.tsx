import * as stylex from "@stylexjs/stylex";
import { useRouter } from "@tanstack/react-router";
import { useState } from "react";
import { PalettePrompt, paletteStyles } from "#/components/ui/command-palette";
import { Dialog } from "#/components/ui/dialog";
import { doCreateWorkspace } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

export function CreateWorkspaceDialog({ onClose }: { onClose: () => void }) {
	const router = useRouter();
	const [name, setName] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();

	const submit = async () => {
		if (!name.trim() || busy) return;
		setBusy(true);
		setError(undefined);
		try {
			const workspace = await doCreateWorkspace({ data: { name } });
			onClose();
			await router.navigate({
				to: "/dashboard",
				search: { workspace: workspace.id },
			});
			await router.invalidate();
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<Dialog label="New workspace" onClose={onClose}>
			<div {...stylex.props(paletteStyles.card)}>
				<PalettePrompt
					crumbs={["New workspace"]}
					label="Workspace name"
					placeholder="Acme"
					value={name}
					busy={busy}
					error={error}
					hint="Group projects and invite people to work on them · Enter to create"
					onChange={(value) => {
						setName(value);
						setError(undefined);
					}}
					onSubmit={() => void submit()}
					onBack={onClose}
				/>
			</div>
		</Dialog>
	);
}
