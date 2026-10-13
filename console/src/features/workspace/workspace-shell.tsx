import * as stylex from "@stylexjs/stylex";
import { Link, useRouter } from "@tanstack/react-router";
import {
	Check,
	ChevronDown,
	History,
	LayoutGrid,
	LogOut,
	Plus,
	Server,
	Settings,
	Users,
	Zap,
} from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { Button } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";
import { UserAvatar, WorkspaceMark } from "#/features/workspace/avatars";
import { CreateWorkspaceDialog } from "#/features/workspace/create-workspace-dialog";
import { roleLabel } from "#/features/workspace/roles";
import type {
	DashboardWorkspaceInvitation,
	DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import { doAnswerWorkspaceInvitation } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

export type WorkspaceSection = "projects" | "people" | "settings";

/** Sidebar layout for workspace-level pages. */
export function WorkspaceShell({
	shell,
	section,
	children,
}: {
	shell: DashboardWorkspaceShell;
	section: WorkspaceSection;
	children: ReactNode;
}) {
	const [creating, setCreating] = useState(false);
	return (
		<div {...stylex.props(styles.layout)}>
			<aside {...stylex.props(styles.sidebar)}>
				<Link to="/dashboard" {...stylex.props(styles.brand)}>
					<Zap size={15} {...stylex.props(styles.brandIcon)} />
					<span {...stylex.props(styles.brandName)}>mesh</span>
				</Link>
				<WorkspaceSwitcher
					shell={shell}
					onCreateWorkspace={() => setCreating(true)}
				/>
				<nav {...stylex.props(styles.nav)} aria-label="Workspace">
					<NavLink
						to="/dashboard"
						active={section === "projects"}
						icon={<LayoutGrid size={14} />}
						label="Projects"
					/>
					<NavLink
						to="/workspace/people"
						active={section === "people"}
						icon={<Users size={14} />}
						label="People"
					/>
					<NavLink
						to="/workspace/settings"
						active={section === "settings"}
						icon={<Settings size={14} />}
						label="Settings"
					/>
				</nav>
				<nav {...stylex.props(styles.nav, styles.secondaryNav)}>
					<NavLink
						to="/deleted"
						active={false}
						icon={<History size={14} />}
						label="Recently deleted"
					/>
					{shell.canManageFleet && (
						<NavLink
							to="/fleet"
							active={false}
							icon={<Server size={14} />}
							label="Fleet"
						/>
					)}
				</nav>
				<div {...stylex.props(styles.spacer)} />
				<div {...stylex.props(styles.account)}>
					<UserAvatar
						email={shell.user.email}
						githubLogin={shell.githubLogin}
					/>
					<span {...stylex.props(styles.accountEmail)}>{shell.user.email}</span>
					<a href="/logout" title="Sign out" {...stylex.props(styles.signOut)}>
						<LogOut size={13} />
					</a>
				</div>
			</aside>
			<main {...stylex.props(styles.main)}>
				{shell.invitations.map((invitation) => (
					<InvitationBanner key={invitation.id} invitation={invitation} />
				))}
				{children}
			</main>
			{creating && <CreateWorkspaceDialog onClose={() => setCreating(false)} />}
		</div>
	);
}

function NavLink({
	to,
	active,
	icon,
	label,
}: {
	to:
		| "/dashboard"
		| "/workspace/people"
		| "/workspace/settings"
		| "/deleted"
		| "/fleet";
	active: boolean;
	icon: ReactNode;
	label: string;
}) {
	return (
		<Link
			to={to}
			aria-current={active ? "page" : undefined}
			{...stylex.props(styles.navLink, active && styles.navLinkActive)}
		>
			{icon}
			{label}
		</Link>
	);
}

function WorkspaceSwitcher({
	shell,
	onCreateWorkspace,
}: {
	shell: DashboardWorkspaceShell;
	onCreateWorkspace: () => void;
}) {
	const [open, setOpen] = useState(false);
	const rootRef = useRef<HTMLDivElement>(null);
	const { workspace } = shell;

	useEffect(() => {
		if (!open) return;
		const onPointerDown = (event: MouseEvent) => {
			if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
		};
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key === "Escape") setOpen(false);
		};
		document.addEventListener("mousedown", onPointerDown);
		document.addEventListener("keydown", onKeyDown);
		return () => {
			document.removeEventListener("mousedown", onPointerDown);
			document.removeEventListener("keydown", onKeyDown);
		};
	}, [open]);

	return (
		<div {...stylex.props(styles.switcher)} ref={rootRef}>
			<button
				type="button"
				aria-haspopup="menu"
				aria-expanded={open}
				aria-label={`Workspace ${workspace.name}`}
				onClick={() => setOpen((current) => !current)}
				{...stylex.props(styles.switcherTrigger)}
			>
				<WorkspaceMark name={workspace.name} />
				<span {...stylex.props(styles.switcherText)}>
					<span {...stylex.props(styles.switcherName)}>{workspace.name}</span>
					<span {...stylex.props(styles.switcherRole)}>
						{workspace.personal ? "Personal" : roleLabel(workspace.role)}
					</span>
				</span>
				<ChevronDown size={13} {...stylex.props(styles.muted)} />
			</button>
			{open && (
				<div role="menu" {...stylex.props(styles.menu)}>
					{shell.workspaces.map((entry) => (
						<Link
							key={entry.id}
							to="/dashboard"
							search={{ workspace: entry.id }}
							role="menuitem"
							onClick={() => setOpen(false)}
							{...stylex.props(styles.menuItem)}
						>
							<WorkspaceMark name={entry.name} />
							<span {...stylex.props(styles.switcherName)}>{entry.name}</span>
							{entry.id === workspace.id && (
								<Check size={13} {...stylex.props(styles.check)} />
							)}
						</Link>
					))}
					<button
						type="button"
						role="menuitem"
						onClick={() => {
							setOpen(false);
							onCreateWorkspace();
						}}
						{...stylex.props(styles.menuItem, styles.menuAction)}
					>
						<Plus size={14} {...stylex.props(styles.check)} />
						New workspace
					</button>
				</div>
			)}
		</div>
	);
}

function InvitationBanner({
	invitation,
}: {
	invitation: DashboardWorkspaceInvitation;
}) {
	const router = useRouter();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();

	const answer = async (accept: boolean) => {
		setBusy(true);
		setError(undefined);
		try {
			await doAnswerWorkspaceInvitation({
				data: { invitationId: invitation.id, accept },
			});
			if (accept) {
				await router.navigate({
					to: "/dashboard",
					search: { workspace: invitation.workspaceId },
				});
			} else {
				await router.invalidate();
			}
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<section
			{...stylex.props(styles.invitation)}
			aria-label="Workspace invitation"
		>
			<WorkspaceMark name={invitation.workspaceName ?? ""} />
			<p {...stylex.props(styles.invitationText)}>
				You're invited to join <strong>{invitation.workspaceName}</strong> as{" "}
				{roleLabel(invitation.role).toLowerCase()}.
			</p>
			{error && <span {...stylex.props(noticeStyles.error)}>{error}</span>}
			<Button
				variant="ghost"
				disabled={busy}
				onClick={() => void answer(false)}
			>
				Decline
			</Button>
			<Button
				variant="primary"
				disabled={busy}
				onClick={() => void answer(true)}
			>
				Accept
			</Button>
		</section>
	);
}

const slideDown = stylex.keyframes({
	from: { opacity: 0, transform: "translateY(-6px)" },
	to: { opacity: 1, transform: "translateY(0)" },
});

const styles = stylex.create({
	layout: {
		display: "flex",
		minHeight: "100dvh",
		flexDirection: { default: "row", "@media (max-width: 767px)": "column" },
		backgroundColor: colors.canvas,
	},
	sidebar: {
		position: { default: "sticky", "@media (max-width: 767px)": "static" },
		top: 0,
		display: "flex",
		width: { default: "240px", "@media (max-width: 767px)": "100%" },
		height: { default: "100dvh", "@media (max-width: 767px)": "auto" },
		flexShrink: 0,
		flexDirection: "column",
		gap: space.lg,
		borderRightStyle: "solid",
		borderRightWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		padding: space.lg,
	},
	brand: {
		display: "flex",
		alignItems: "center",
		gap: "0.625rem",
		paddingInline: space.xs,
		textDecoration: "none",
	},
	brandIcon: { color: colors.accent },
	brandName: {
		fontFamily: fonts.condensed,
		fontSize: "1rem",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.14em",
		color: colors.ink,
	},
	switcher: { position: "relative" },
	switcherTrigger: {
		display: "flex",
		width: "100%",
		cursor: "pointer",
		alignItems: "center",
		gap: space.sm,
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
		},
		backgroundColor: colors.surfaceRaised,
		padding: space.sm,
		textAlign: "left",
		color: colors.ink,
		transitionProperty: "border-color",
		transitionDuration: motion.fast,
	},
	switcherText: {
		display: "flex",
		minWidth: 0,
		flex: 1,
		flexDirection: "column",
	},
	switcherName: {
		overflow: "hidden",
		flex: 1,
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontSize: "13px",
		fontWeight: "600",
	},
	switcherRole: {
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.accent,
	},
	muted: { flexShrink: 0, color: colors.muted },
	menu: {
		position: "absolute",
		top: "calc(100% + 6px)",
		insetInline: 0,
		zIndex: 60,
		display: "flex",
		flexDirection: "column",
		animation: `${slideDown} 0.16s cubic-bezier(0.22, 1, 0.36, 1)`,
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: colors.surface,
		paddingBlock: "0.375rem",
		boxShadow: "0 18px 50px rgba(0,0,0,0.55)",
	},
	menuItem: {
		display: "flex",
		cursor: "pointer",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "none",
		backgroundColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		paddingInline: space.md,
		paddingBlock: space.sm,
		fontFamily: fonts.sans,
		fontSize: "13px",
		color: colors.ink,
		textAlign: "left",
		textDecoration: "none",
	},
	menuAction: {
		marginTop: "0.375rem",
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderTopColor: colors.line,
		paddingTop: space.md,
		color: colors.muted,
	},
	check: { flexShrink: 0, color: colors.accent },
	nav: { display: "flex", flexDirection: "column", gap: "2px" },
	secondaryNav: {
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderTopColor: colors.line,
		paddingTop: space.lg,
	},
	navLink: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderRadius: shape.card,
		borderLeftStyle: "solid",
		borderLeftWidth: "2px",
		borderLeftColor: "transparent",
		paddingInline: space.sm,
		paddingBlock: "0.4375rem",
		fontSize: "13px",
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
		textDecoration: "none",
		transitionProperty: "color, background-color",
		transitionDuration: motion.fast,
	},
	navLinkActive: {
		borderLeftColor: colors.accent,
		backgroundColor: colors.surfaceRaised,
		color: colors.ink,
	},
	spacer: { flex: 1 },
	account: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderTopColor: colors.line,
		paddingTop: space.lg,
	},
	accountEmail: {
		overflow: "hidden",
		flex: 1,
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontSize: "12px",
		color: colors.muted,
	},
	signOut: {
		display: "inline-flex",
		padding: space.xs,
		color: {
			default: colors.dim,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
	},
	main: {
		display: "flex",
		minWidth: 0,
		flex: 1,
		flexDirection: "column",
		gap: space.xl,
		marginInline: "auto",
		width: "100%",
		maxWidth: "1120px",
		paddingInline: { default: "3rem", "@media (max-width: 767px)": space.lg },
		paddingBlock: { default: "3rem", "@media (max-width: 767px)": space.xl },
	},
	invitation: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.accentGlow,
		borderRadius: shape.card,
		backgroundColor: colors.accentDim,
		padding: space.md,
	},
	invitationText: {
		margin: 0,
		flex: 1,
		fontSize: "13px",
		color: colors.ink,
	},
});
