import * as stylex from "@stylexjs/stylex";
import { useRouter } from "@tanstack/react-router";
import { LogOut, Trash2 } from "lucide-react";
import { type FormEvent, useId, useState } from "react";
import { DeleteResourceDialog } from "#/components/delete-resource-dialog";
import { Button } from "#/components/ui/button";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { Notice } from "#/components/ui/notice";
import { SettingsCard } from "#/features/project-settings/settings-card";
import { WorkspaceMark } from "#/features/workspace/avatars";
import { WorkspacePageHeader } from "#/features/workspace/page-header";
import { canManageWorkspace, roleLabel } from "#/features/workspace/roles";
import type { DashboardWorkspaceShell } from "#/lib/dashboard/core/types.server";
import {
	doDeleteWorkspace,
	doRemoveWorkspaceMember,
	doRenameWorkspace,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function WorkspaceSettingsPage({
	shell,
}: {
	shell: DashboardWorkspaceShell;
}) {
	const router = useRouter();
	const { workspace } = shell;
	const manage = canManageWorkspace(workspace);
	const nameId = useId();
	const [name, setName] = useState(workspace.name);
	const [saving, setSaving] = useState(false);
	const [saveError, setSaveError] = useState<string>();
	const [deleting, setDeleting] = useState(false);
	const [deleteBusy, setDeleteBusy] = useState(false);
	const [dangerError, setDangerError] = useState<string>();

	const rename = async (event: FormEvent) => {
		event.preventDefault();
		setSaving(true);
		setSaveError(undefined);
		try {
			await doRenameWorkspace({ data: { workspaceId: workspace.id, name } });
			await router.invalidate();
		} catch (cause) {
			setSaveError(formatError(cause));
		} finally {
			setSaving(false);
		}
	};

	const leave = async () => {
		if (
			!window.confirm(
				`Leave ${workspace.name}? You lose access to its projects.`,
			)
		)
			return;
		setDangerError(undefined);
		try {
			await doRemoveWorkspaceMember({
				data: { workspaceId: workspace.id, userId: shell.user.id },
			});
			await router.navigate({ to: "/dashboard" });
			await router.invalidate();
		} catch (cause) {
			setDangerError(formatError(cause));
		}
	};

	const remove = async (confirmationName: string) => {
		setDeleteBusy(true);
		setDangerError(undefined);
		try {
			await doDeleteWorkspace({
				data: { workspaceId: workspace.id, confirmationName },
			});
			setDeleting(false);
			await router.navigate({ to: "/dashboard" });
			await router.invalidate();
		} catch (cause) {
			setDangerError(formatError(cause));
			setDeleteBusy(false);
		}
	};

	return (
		<>
			<WorkspacePageHeader
				title="Settings"
				lede={`General settings for ${workspace.name}`}
			/>

			<SettingsCard
				title="Workspace"
				lede={
					manage
						? "The name everyone in this workspace sees."
						: `Only admins can change the workspace. Your role is ${roleLabel(workspace.role).toLowerCase()}.`
				}
			>
				<form
					onSubmit={(event) => void rename(event)}
					{...stylex.props(styles.form)}
				>
					<WorkspaceMark name={name || workspace.name} large />
					<label htmlFor={nameId} {...stylex.props(styles.field)}>
						<span {...stylex.props(fieldStyles.label)}>Name</span>
						<TextInput
							id={nameId}
							value={name}
							maxLength={64}
							disabled={!manage || saving}
							onChange={(event) => setName(event.target.value)}
						/>
					</label>
					{manage && (
						<Button
							type="submit"
							variant="primary"
							disabled={
								saving || !name.trim() || name.trim() === workspace.name
							}
						>
							Save
						</Button>
					)}
				</form>
				{saveError && <Notice>{saveError}</Notice>}
				<p {...stylex.props(styles.id)}>{workspace.id}</p>
			</SettingsCard>

			{workspace.personal ? (
				<SettingsCard
					title="Personal workspace"
					lede="Every account has one personal workspace. It cannot be deleted, left, or shared."
				>
					<p {...stylex.props(styles.dangerDescription)}>
						To work with others, create a workspace from the workspace menu and
						move projects into it from their settings.
					</p>
				</SettingsCard>
			) : (
				<SettingsCard title="Danger zone" tone="danger">
					<div {...stylex.props(styles.dangerRow)}>
						<div>
							<strong {...stylex.props(styles.dangerTitle)}>
								Leave this workspace
							</strong>
							<span {...stylex.props(styles.dangerDescription)}>
								You lose access to its projects. A workspace keeps at least one
								admin.
							</span>
						</div>
						<Button variant="dangerOutline" onClick={() => void leave()}>
							<LogOut size={13} />
							Leave workspace
						</Button>
					</div>
					{manage && (
						<div {...stylex.props(styles.dangerRow)}>
							<div>
								<strong {...stylex.props(styles.dangerTitle)}>
									Delete this workspace
								</strong>
								<span {...stylex.props(styles.dangerDescription)}>
									Removes the workspace, its members, and pending invitations.
									Move or delete its projects first. Recently deleted projects
									in it can no longer be restored.
								</span>
							</div>
							<Button
								variant="dangerOutline"
								onClick={() => {
									setDangerError(undefined);
									setDeleting(true);
								}}
							>
								<Trash2 size={13} />
								Delete workspace
							</Button>
						</div>
					)}
					{dangerError && !deleting && <Notice>{dangerError}</Notice>}
				</SettingsCard>
			)}

			{deleting && (
				<DeleteResourceDialog
					title="Delete Workspace"
					name={workspace.name}
					recovery="permanent"
					requireName
					busy={deleteBusy}
					error={dangerError}
					description={
						<>
							You are deleting the workspace <strong>{workspace.name}</strong>.
							Everyone in it loses access, and its recently deleted projects are
							destroyed instead of waiting out their grace period.
						</>
					}
					onCancel={() => {
						if (!deleteBusy) setDeleting(false);
					}}
					onConfirm={(confirmationName) => void remove(confirmationName)}
				/>
			)}
		</>
	);
}

const styles = stylex.create({
	form: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "flex-end",
		gap: space.md,
	},
	field: { flex: "1 1 16rem" },
	id: {
		margin: 0,
		fontFamily: fonts.mono,
		fontSize: "0.75rem",
		color: colors.dim,
	},
	dangerRow: {
		display: "flex",
		alignItems: { default: "center", "@media (width < 40rem)": "flex-start" },
		justifyContent: "space-between",
		gap: space.lg,
		flexDirection: { default: null, "@media (width < 40rem)": "column" },
	},
	dangerTitle: {
		marginBottom: "3px",
		display: "block",
		fontSize: "13px",
		fontWeight: "600",
		color: colors.ink,
	},
	dangerDescription: {
		display: "block",
		fontSize: "0.75rem",
		lineHeight: "1.5",
		color: colors.muted,
	},
});
