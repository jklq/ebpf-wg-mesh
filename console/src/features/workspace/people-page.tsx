import * as stylex from "@stylexjs/stylex";
import { useRouter } from "@tanstack/react-router";
import { Plus, UserMinus, X } from "lucide-react";
import { useState } from "react";
import { Button } from "#/components/ui/button";
import { fieldStyles } from "#/components/ui/field";
import { Notice } from "#/components/ui/notice";
import { UserAvatar } from "#/features/workspace/avatars";
import { CreateWorkspaceDialog } from "#/features/workspace/create-workspace-dialog";
import { InviteDialog } from "#/features/workspace/invite-dialog";
import { WorkspacePageHeader } from "#/features/workspace/page-header";
import {
	canManageWorkspace,
	roleLabel,
	workspaceRoles,
} from "#/features/workspace/roles";
import { WorkspaceShell } from "#/features/workspace/workspace-shell";
import type {
	DashboardWorkspaceInvitation,
	DashboardWorkspaceMember,
	DashboardWorkspaceRole,
	DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import {
	doRemoveWorkspaceMember,
	doRevokeWorkspaceInvitation,
	doUpdateWorkspaceMemberRole,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { formatRelativeTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function PeoplePage({
	shell,
	members,
	invitations,
	nowMs,
}: {
	shell: DashboardWorkspaceShell;
	members: Array<DashboardWorkspaceMember>;
	invitations: Array<DashboardWorkspaceInvitation>;
	nowMs: number;
}) {
	const router = useRouter();
	const { workspace } = shell;
	const manage = canManageWorkspace(workspace);
	const [inviting, setInviting] = useState(false);
	const [creatingWorkspace, setCreatingWorkspace] = useState(false);
	const [busyKey, setBusyKey] = useState<string>();
	const [error, setError] = useState<string>();

	const run = async (key: string, action: () => Promise<unknown>) => {
		setBusyKey(key);
		setError(undefined);
		try {
			await action();
			await router.invalidate();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusyKey(undefined);
		}
	};

	const changeRole = (
		member: DashboardWorkspaceMember,
		role: DashboardWorkspaceRole,
	) =>
		run(member.userId, () =>
			doUpdateWorkspaceMemberRole({
				data: { workspaceId: workspace.id, userId: member.userId, role },
			}),
		);

	const remove = (member: DashboardWorkspaceMember) => {
		const self = member.userId === shell.user.id;
		const accepted = window.confirm(
			self
				? `Leave ${workspace.name}? You lose access to its projects.`
				: `Remove ${member.email ?? member.userId} from ${workspace.name}?`,
		);
		if (!accepted) return;
		void run(member.userId, async () => {
			await doRemoveWorkspaceMember({
				data: { workspaceId: workspace.id, userId: member.userId },
			});
			if (self) await router.navigate({ to: "/dashboard" });
		});
	};

	return (
		<WorkspaceShell shell={shell} section="people">
			<WorkspacePageHeader
				title="People"
				lede={`${members.length} ${members.length === 1 ? "person has" : "people have"} access to ${workspace.name}`}
				actions={
					manage &&
					!workspace.personal && (
						<Button variant="primary" onClick={() => setInviting(true)}>
							<Plus size={13} />
							Invite
						</Button>
					)
				}
			/>

			{workspace.personal && (
				<div {...stylex.props(styles.personalNote)}>
					<p {...stylex.props(styles.personalText)}>
						<strong>{workspace.name}</strong> is your personal workspace and
						only you can access it. To work with others, create a shared
						workspace and invite them there. You can move projects between
						workspaces from project settings.
					</p>
					<Button onClick={() => setCreatingWorkspace(true)}>
						<Plus size={13} />
						New workspace
					</Button>
				</div>
			)}

			{error && <Notice>{error}</Notice>}

			<table {...stylex.props(styles.table)}>
				<thead>
					<tr>
						<th {...stylex.props(styles.heading)}>Person</th>
						<th {...stylex.props(styles.heading)}>Role</th>
						<th {...stylex.props(styles.heading)}>Joined</th>
						<th {...stylex.props(styles.heading)}>
							<span {...stylex.props(styles.hidden)}>Actions</span>
						</th>
					</tr>
				</thead>
				<tbody>
					{members.map((member) => {
						const self = member.userId === shell.user.id;
						const busy = busyKey === member.userId;
						return (
							<tr key={member.userId} {...stylex.props(styles.row)}>
								<td {...stylex.props(styles.cell)}>
									<span {...stylex.props(styles.person)}>
										<UserAvatar
											email={member.email}
											githubLogin={member.githubLogin}
										/>
										<span {...stylex.props(styles.email)}>
											{member.email ?? member.userId}
										</span>
										{self && <span {...stylex.props(styles.you)}>You</span>}
									</span>
								</td>
								<td {...stylex.props(styles.cell)}>
									{manage && !workspace.personal ? (
										<select
											aria-label={`Role for ${member.email ?? member.userId}`}
											value={member.role}
											disabled={busy}
											onChange={(event) =>
												void changeRole(
													member,
													event.target.value as DashboardWorkspaceRole,
												)
											}
											{...stylex.props(fieldStyles.input, styles.roleSelect)}
										>
											{workspaceRoles.map((entry) => (
												<option key={entry.role} value={entry.role}>
													{entry.label}
												</option>
											))}
										</select>
									) : (
										roleLabel(member.role)
									)}
								</td>
								<td {...stylex.props(styles.cell, styles.dim)}>
									{formatRelativeTime(member.joinedAt, nowMs, "—")}
								</td>
								<td {...stylex.props(styles.cell, styles.actionCell)}>
									{!workspace.personal && (manage || self) && (
										<Button
											variant="ghost"
											disabled={busy}
											onClick={() => remove(member)}
										>
											<UserMinus size={13} />
											{self ? "Leave" : "Remove"}
										</Button>
									)}
								</td>
							</tr>
						);
					})}
				</tbody>
			</table>

			{invitations.length > 0 && (
				<section {...stylex.props(styles.section)}>
					<h2 {...stylex.props(styles.sectionTitle)}>Pending invitations</h2>
					<table {...stylex.props(styles.table)}>
						<tbody>
							{invitations.map((invitation) => (
								<tr key={invitation.id} {...stylex.props(styles.row)}>
									<td {...stylex.props(styles.cell)}>
										<span {...stylex.props(styles.person)}>
											<UserAvatar email={invitation.email} />
											<span {...stylex.props(styles.email)}>
												{invitation.email}
											</span>
										</span>
									</td>
									<td {...stylex.props(styles.cell)}>
										{roleLabel(invitation.role)}
									</td>
									<td {...stylex.props(styles.cell, styles.dim)}>
										Invited{" "}
										{formatRelativeTime(invitation.createdAt, nowMs, "")}
									</td>
									<td {...stylex.props(styles.cell, styles.actionCell)}>
										{manage && (
											<Button
												variant="ghost"
												disabled={busyKey === invitation.id}
												onClick={() =>
													void run(invitation.id, () =>
														doRevokeWorkspaceInvitation({
															data: { invitationId: invitation.id },
														}),
													)
												}
											>
												<X size={13} />
												Revoke
											</Button>
										)}
									</td>
								</tr>
							))}
						</tbody>
					</table>
				</section>
			)}

			{inviting && (
				<InviteDialog
					workspace={workspace}
					onClose={() => setInviting(false)}
				/>
			)}
			{creatingWorkspace && (
				<CreateWorkspaceDialog onClose={() => setCreatingWorkspace(false)} />
			)}
		</WorkspaceShell>
	);
}

const styles = stylex.create({
	personalNote: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: space.lg,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.accentGlow,
		backgroundColor: colors.accentDim,
		padding: space.lg,
	},
	personalText: {
		margin: 0,
		flex: "1 1 24rem",
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.label,
	},
	table: {
		width: "100%",
		borderCollapse: "collapse",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
	},
	heading: {
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderBottomColor: colors.line,
		paddingInline: space.lg,
		paddingBlock: space.md,
		textAlign: "left",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.muted,
	},
	hidden: {
		position: "absolute",
		width: "1px",
		height: "1px",
		overflow: "hidden",
		clip: "rect(0 0 0 0)",
	},
	row: {
		borderBottomStyle: "solid",
		borderBottomWidth: { default: "1px", ":last-child": "0px" },
		borderBottomColor: colors.line,
	},
	cell: {
		paddingInline: space.lg,
		paddingBlock: space.md,
		fontSize: "13px",
		color: colors.ink,
		verticalAlign: "middle",
	},
	dim: { color: colors.muted },
	actionCell: { width: "1%", whiteSpace: "nowrap", textAlign: "right" },
	person: { display: "flex", alignItems: "center", gap: space.md },
	email: { overflow: "hidden", textOverflow: "ellipsis" },
	you: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: "0.375rem",
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.09em",
		color: colors.muted,
	},
	roleSelect: { width: "auto", minWidth: "8rem", fontFamily: fonts.sans },
	section: { display: "flex", flexDirection: "column", gap: space.md },
	sectionTitle: {
		margin: 0,
		fontFamily: fonts.display,
		fontSize: "20px",
		fontWeight: "500",
		color: colors.ink,
	},
});
