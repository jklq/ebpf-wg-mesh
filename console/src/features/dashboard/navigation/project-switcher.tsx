import * as stylex from "@stylexjs/stylex";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

const slideDown = stylex.keyframes({
	from: { opacity: "0", transform: "translateY(-6px)" },
	to: { opacity: "1", transform: "translateY(0)" },
});

import { Link } from "@tanstack/react-router";
import { Check, ChevronDown, History, Layers, Settings } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

const menuLink = stylex.create({
	root: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		paddingInline: space.md,
		paddingBlock: space.sm,
		fontFamily: fonts.sans,
		fontSize: "13px",
		color: colors.ink,
		textDecorationLine: "none",
		backgroundColor: {
			default: null,
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
	},
});

export function ProjectSwitcher({ state }: { state: DashboardHomeState }) {
	const [open, setOpen] = useState(false);
	const rootRef = useRef<HTMLDivElement>(null);
	const project = state.project;

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

	if (!project) return null;
	const projects = state.projects.some((entry) => entry.id === project.id)
		? state.projects
		: [project, ...state.projects];

	return (
		<div {...stylex.props(styles.root)} ref={rootRef}>
			<button
				type="button"
				{...stylex.props([styles.trigger, open && styles.openTrigger])}
				aria-haspopup="menu"
				aria-expanded={open}
				aria-label="Project"
				onClick={() => setOpen((current) => !current)}
			>
				<Layers size={12} {...stylex.props(styles.projectIcon)} />
				<span {...stylex.props(styles.projectName)}>{project.name}</span>
				<ChevronDown
					size={12}
					{...stylex.props([styles.projectIcon, open && styles.openChevron])}
				/>
			</button>

			{open && (
				<div {...stylex.props(styles.menu)} role="menu">
					<p {...stylex.props(styles.menuLabel)}>Projects</p>
					<div {...stylex.props(styles.projectList)}>
						{projects.map((entry) => {
							const active = entry.id === project.id;
							return (
								<Link
									key={entry.id}
									to="/projects/$projectId"
									params={{ projectId: entry.id }}
									role="menuitem"
									{...stylex.props([
										menuLink.root,
										styles.projectLink,
										active && styles.selectedProject,
									])}
									onClick={() => setOpen(false)}
								>
									<span {...stylex.props(styles.selectionIndicator)}>
										{active && <Check size={13} />}
									</span>
									<span {...stylex.props(styles.projectName)}>
										{entry.name}
									</span>
								</Link>
							);
						})}
					</div>
					<div {...stylex.props(styles.menuFooter)}>
						<Link
							to="/projects/$projectId/settings"
							params={{ projectId: project.id }}
							role="menuitem"
							{...stylex.props(menuLink.root)}
							onClick={() => setOpen(false)}
						>
							<Settings size={13} {...stylex.props(styles.menuIcon)} />
							Project settings
						</Link>
						<Link
							to="/deleted"
							role="menuitem"
							{...stylex.props(menuLink.root)}
							onClick={() => setOpen(false)}
						>
							<History size={13} {...stylex.props(styles.menuIcon)} />
							Recently deleted
						</Link>
					</div>
				</div>
			)}
		</div>
	);
}

const styles = stylex.create({
	root: { position: "relative" },
	trigger: {
		display: "inline-flex",
		maxWidth: "220px",
		cursor: "pointer",
		alignItems: "center",
		gap: "0.375rem",
		whiteSpace: "nowrap",
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
		},
		backgroundColor: colors.surfaceRaised,
		paddingInline: space.sm,
		paddingBlock: "3px",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "600",
		textTransform: "uppercase",
		letterSpacing: "0.06em",
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
		transitionProperty:
			"color, background-color, border-color, outline-color, text-decoration-color, fill, stroke",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
	},
	openTrigger: { borderColor: colors.lineBright, color: colors.ink },
	projectIcon: { flexShrink: "0" },
	projectName: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	openChevron: { rotate: "180deg" },
	menu: {
		position: "absolute",
		top: "calc(100% + 8px)",
		left: "0rem",
		zIndex: "60",
		minWidth: "240px",
		animation: `${slideDown} 0.16s cubic-bezier(0.22, 1, 0.36, 1)`,
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: colors.surface,
		paddingBlock: "0.375rem",
		boxShadow: "0 18px 50px rgba(0,0,0,0.55)",
	},
	menuLabel: {
		margin: "0rem",
		paddingInline: space.md,
		paddingTop: "0.375rem",
		paddingBottom: space.sm,
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		letterSpacing: "0.1em",
		color: colors.muted,
		textTransform: "uppercase",
	},
	projectList: {
		display: "flex",
		maxHeight: "18rem",
		flexDirection: "column",
		overflowY: "auto",
	},
	projectLink: { paddingLeft: "0.625rem" },
	selectedProject: { fontWeight: "600" },
	selectionIndicator: {
		display: "inline-flex",
		width: "0.875rem",
		flexShrink: "0",
		alignItems: "center",
		justifyContent: "center",
		color: colors.accent,
	},
	menuFooter: {
		marginTop: "0.375rem",
		display: "flex",
		flexDirection: "column",
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: colors.line,
		paddingTop: "0.375rem",
	},
	menuIcon: { color: colors.muted },
});
