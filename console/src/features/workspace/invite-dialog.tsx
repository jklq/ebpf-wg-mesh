import * as stylex from "@stylexjs/stylex";
import { useRouter } from "@tanstack/react-router";
import { X } from "lucide-react";
import { type FormEvent, useId, useState } from "react";
import { Button } from "#/components/ui/button";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { Notice } from "#/components/ui/notice";
import { workspaceRoles } from "#/features/workspace/roles";
import type {
	DashboardWorkspace,
	DashboardWorkspaceRole,
} from "#/lib/dashboard/core/types.server";
import { doInviteWorkspaceMember } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function InviteDialog({
	workspace,
	onClose,
}: {
	workspace: DashboardWorkspace;
	onClose: () => void;
}) {
	const router = useRouter();
	const emailId = useId();
	const [email, setEmail] = useState("");
	const [role, setRole] = useState<DashboardWorkspaceRole>(
		"WORKSPACE_ROLE_MEMBER",
	);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();

	const submit = async (event: FormEvent) => {
		event.preventDefault();
		setBusy(true);
		setError(undefined);
		try {
			await doInviteWorkspaceMember({
				data: { workspaceId: workspace.id, email, role },
			});
			await router.invalidate();
			onClose();
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<Dialog label="Invite to workspace" onClose={busy ? undefined : onClose}>
			<form
				onSubmit={(event) => void submit(event)}
				{...stylex.props(dialogStyles.card, styles.form)}
			>
				<header {...stylex.props(styles.header)}>
					<h2 {...stylex.props(styles.title)}>Invite to {workspace.name}</h2>
					<Button
						variant="icon"
						title="Close"
						onClick={onClose}
						disabled={busy}
					>
						<X size={14} />
					</Button>
				</header>
				<p {...stylex.props(styles.lede)}>
					They join when they sign in with this email address and accept.
				</p>
				<label htmlFor={emailId}>
					<span {...stylex.props(fieldStyles.label)}>Email</span>
					<TextInput
						id={emailId}
						type="email"
						required
						autoFocus
						placeholder="name@company.com"
						value={email}
						disabled={busy}
						onChange={(event) => setEmail(event.target.value)}
					/>
				</label>
				<fieldset {...stylex.props(styles.roles)}>
					<legend {...stylex.props(fieldStyles.label)}>Workspace role</legend>
					{workspaceRoles.map((entry) => (
						<label
							key={entry.role}
							{...stylex.props(
								styles.roleOption,
								role === entry.role && styles.roleSelected,
							)}
						>
							<input
								type="radio"
								name="role"
								value={entry.role}
								checked={role === entry.role}
								disabled={busy}
								onChange={() => setRole(entry.role)}
								{...stylex.props(styles.radio)}
							/>
							<span>
								<strong {...stylex.props(styles.roleName)}>
									{entry.label}
								</strong>
								<span {...stylex.props(styles.roleDescription)}>
									{entry.description}
								</span>
							</span>
						</label>
					))}
				</fieldset>
				{error && <Notice>{error}</Notice>}
				<div {...stylex.props(styles.actions)}>
					<Button onClick={onClose} disabled={busy}>
						Cancel
					</Button>
					<Button
						type="submit"
						variant="primary"
						disabled={busy || !email.trim()}
					>
						Send invitation
					</Button>
				</div>
			</form>
		</Dialog>
	);
}

const styles = stylex.create({
	form: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		padding: space.xl,
	},
	header: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
	},
	title: {
		margin: 0,
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
		color: colors.ink,
	},
	lede: {
		margin: 0,
		marginTop: `calc(-1 * ${space.sm})`,
		fontSize: "13px",
		color: colors.muted,
	},
	roles: {
		margin: 0,
		display: "flex",
		flexDirection: "column",
		gap: space.sm,
		borderStyle: "none",
		padding: 0,
	},
	roleOption: {
		display: "flex",
		cursor: "pointer",
		alignItems: "flex-start",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		padding: space.md,
	},
	roleSelected: {
		borderColor: colors.accent,
		backgroundColor: colors.accentDim,
	},
	radio: { marginTop: "3px", accentColor: colors.accent },
	roleName: { display: "block", fontSize: "13px", color: colors.ink },
	roleDescription: { display: "block", fontSize: "12px", color: colors.muted },
	actions: { display: "flex", justifyContent: "flex-end", gap: space.sm },
});
