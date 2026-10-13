import * as stylex from "@stylexjs/stylex";
import { useRouter } from "@tanstack/react-router";
import { ArrowRightLeft } from "lucide-react";
import { useState } from "react";
import { Button } from "#/components/ui/button";
import { fieldStyles } from "#/components/ui/field";
import { Notice } from "#/components/ui/notice";
import type {
	DashboardProject,
	DashboardWorkspace,
} from "#/lib/dashboard/core/types.server";
import { doMoveProject } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, space } from "#/styles/tokens.stylex";

/**
 * Moves the project to another workspace. Requires admin in the current
 * workspace and at least member in the target.
 */
export function MoveProjectForm({
	project,
	workspaces,
}: {
	project: DashboardProject;
	workspaces: Array<DashboardWorkspace>;
}) {
	const router = useRouter();
	const current = workspaces.find((entry) => entry.id === project.workspaceId);
	const targets = workspaces.filter(
		(entry) =>
			entry.id !== project.workspaceId &&
			(entry.role === "WORKSPACE_ROLE_ADMIN" ||
				entry.role === "WORKSPACE_ROLE_MEMBER"),
	);
	const [target, setTarget] = useState(targets[0]?.id ?? "");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();

	if (current?.role !== "WORKSPACE_ROLE_ADMIN") {
		return (
			<p {...stylex.props(styles.note)}>
				In <strong>{current?.name ?? "another workspace"}</strong>. Only its
				admins can move this project.
			</p>
		);
	}
	if (targets.length === 0) {
		return (
			<p {...stylex.props(styles.note)}>
				In <strong>{current.name}</strong>. Create or join another workspace to
				move this project there.
			</p>
		);
	}

	const move = async () => {
		const destination = targets.find((entry) => entry.id === target);
		if (
			!destination ||
			!window.confirm(
				`Move ${project.name} to ${destination.name}? People in ${current.name} lose access unless they are also in ${destination.name}.`,
			)
		)
			return;
		setBusy(true);
		setError(undefined);
		try {
			await doMoveProject({
				data: { projectId: project.id, workspaceId: destination.id },
			});
			await router.invalidate();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};

	return (
		<div {...stylex.props(styles.form)}>
			<p {...stylex.props(styles.note)}>
				In <strong>{current.name}</strong>.
			</p>
			<div {...stylex.props(styles.row)}>
				<select
					aria-label="Destination workspace"
					value={target}
					disabled={busy}
					onChange={(event) => setTarget(event.target.value)}
					{...stylex.props(fieldStyles.input, styles.select)}
				>
					{targets.map((entry) => (
						<option key={entry.id} value={entry.id}>
							{entry.name}
						</option>
					))}
				</select>
				<Button disabled={busy || !target} onClick={() => void move()}>
					<ArrowRightLeft size={13} />
					Move project
				</Button>
			</div>
			{error && <Notice>{error}</Notice>}
		</div>
	);
}

const styles = stylex.create({
	form: { display: "flex", flexDirection: "column", gap: space.md },
	row: { display: "flex", flexWrap: "wrap", gap: space.sm },
	select: { width: "auto", minWidth: "14rem", flex: "1 1 14rem" },
	note: { margin: 0, fontSize: "13px", color: colors.muted },
});
