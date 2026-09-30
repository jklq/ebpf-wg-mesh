import * as stylex from "@stylexjs/stylex";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

const slideDown = stylex.keyframes({
	from: { opacity: "0", transform: "translateY(-6px)" },
	to: { opacity: "1", transform: "translateY(0)" },
});

import { Check, ChevronDown, MoreHorizontal, Plus } from "lucide-react";
import type { ReactNode, RefObject } from "react";
import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { DeleteResourceDialog } from "#/components/delete-resource-dialog";
import type {
	DashboardEnvironment,
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";
import {
	doDeleteResource,
	doRenameEnvironment,
	doUpdateEnvironmentAutoDeploy,
	fetchDeletionPreview,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

type RowMenuAnchor = { environmentId: string; top: number; right: number };

const envTag = stylex.create({
	root: {
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.accentGlow,
		backgroundColor: colors.accentDim,
		paddingInline: "5px",
		paddingBlock: "1px",
		fontFamily: fonts.condensed,
		fontSize: "9px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.accent,
	},
});
const manualTag = stylex.create({
	root: {
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		paddingInline: "5px",
		paddingBlock: "1px",
		fontFamily: fonts.condensed,
		fontSize: "9px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.muted,
	},
});
const rowMenuItem = stylex.create({
	root: {
		cursor: { default: "pointer", ":disabled": "not-allowed" },
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: {
			default: "transparent",
			":enabled": {
				default: null,
				":hover": {
					default: null,
					"@media (hover: hover)": colors.surfaceHover,
				},
			},
		},
		paddingInline: space.md,
		paddingBlock: "7px",
		textAlign: "left",
		fontFamily: fonts.sans,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.ink,
		opacity: { default: null, ":disabled": "35%" },
	},
});

export function EnvironmentSwitcher({
	state,
	onCreateEnvironment,
	onChanged,
	onNavigateEnvironment,
}: {
	state: DashboardHomeState;
	onCreateEnvironment: () => void;
	onChanged: () => void;
	onNavigateEnvironment: (environmentId: string | null) => void;
}) {
	const [open, setOpen] = useState(false);
	const [menuFor, setMenuFor] = useState<RowMenuAnchor | null>(null);
	const [renamingId, setRenamingId] = useState<string | null>(null);
	const [renameDraft, setRenameDraft] = useState("");
	const [deleting, setDeleting] = useState<DashboardEnvironment | null>(null);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const rootRef = useRef<HTMLDivElement>(null);
	const rowMenuRef = useRef<HTMLDivElement>(null);
	const renameInputRef = useRef<HTMLInputElement>(null);
	const environment = state.environment;

	const closeMenus = useCallback(() => {
		setOpen(false);
		setMenuFor(null);
		setRenamingId(null);
		setError(undefined);
	}, []);

	useEffect(() => {
		if (!open) return;
		const onPointerDown = (event: MouseEvent) => {
			const target = event.target as Node;
			if (
				!rootRef.current?.contains(target) &&
				!rowMenuRef.current?.contains(target)
			) {
				closeMenus();
			}
		};
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key !== "Escape") return;
			if (deleting) return;
			closeMenus();
		};
		document.addEventListener("mousedown", onPointerDown);
		document.addEventListener("keydown", onKeyDown);
		return () => {
			document.removeEventListener("mousedown", onPointerDown);
			document.removeEventListener("keydown", onKeyDown);
		};
	}, [open, deleting, closeMenus]);

	useEffect(() => {
		if (!menuFor) return;
		const dismiss = () => setMenuFor(null);
		window.addEventListener("scroll", dismiss, true);
		window.addEventListener("resize", dismiss);
		return () => {
			window.removeEventListener("scroll", dismiss, true);
			window.removeEventListener("resize", dismiss);
		};
	}, [menuFor]);

	useEffect(() => {
		if (!renamingId) return;
		const input = renameInputRef.current;
		if (!input) return;
		input.focus();
		input.select();
	}, [renamingId]);

	if (!environment) return null;

	const environments = state.environments.some(
		(entry) => entry.id === environment.id,
	)
		? state.environments
		: [environment, ...state.environments];

	const selectEnvironment = (id: string) => {
		closeMenus();
		if (id === environment.id) return;
		onNavigateEnvironment(id);
	};

	const openRowMenu = (
		target: DashboardEnvironment,
		trigger: HTMLElement | null,
	) => {
		setMenuFor((current) => {
			if (current?.environmentId === target.id) return null;
			const rect = trigger?.getBoundingClientRect();
			return {
				environmentId: target.id,
				top: (rect?.bottom ?? 0) + 4,
				right: Math.max(8, window.innerWidth - (rect?.right ?? 0)),
			};
		});
	};

	const startRename = (target: DashboardEnvironment) => {
		setMenuFor(null);
		setError(undefined);
		setRenameDraft(target.name);
		setRenamingId(target.id);
	};

	const commitRename = async (target: DashboardEnvironment) => {
		const name = renameDraft.trim();
		if (!name || name === target.name) {
			setRenamingId(null);
			return;
		}
		setBusy(true);
		setError(undefined);
		try {
			await doRenameEnvironment({
				data: { environmentId: target.id, name },
			});
			setRenamingId(null);
			onChanged();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};

	const toggleAutoDeploy = async (target: DashboardEnvironment) => {
		setMenuFor(null);
		setBusy(true);
		setError(undefined);
		try {
			await doUpdateEnvironmentAutoDeploy({
				data: { environmentId: target.id, autoDeploy: !target.autoDeploy },
			});
			onChanged();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};

	const confirmDelete = async (confirmationName: string) => {
		if (!deleting) return;
		setBusy(true);
		setError(undefined);
		try {
			await doDeleteResource({
				data: { kind: "environment", id: deleting.id, confirmationName },
			});
			setBusy(false);
			if (deleting.id === environment.id) {
				const fallback = state.environments.find(
					(entry) => entry.id !== deleting.id && entry.isProduction,
				);
				const next =
					fallback ??
					state.environments.find((entry) => entry.id !== deleting.id);
				setDeleting(null);
				closeMenus();
				onNavigateEnvironment(next ? next.id : null);
				return;
			}
			setDeleting(null);
			closeMenus();
			onChanged();
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<div {...stylex.props(styles.root)} ref={rootRef}>
			<button
				type="button"
				{...stylex.props([styles.trigger, open && styles.openTrigger])}
				aria-haspopup="menu"
				aria-expanded={open}
				aria-label="Environment"
				onClick={() => (open ? closeMenus() : setOpen(true))}
			>
				<span {...stylex.props(styles.environmentName)}>
					{environment.name}
				</span>
				{environment.isProduction && (
					<span {...stylex.props(envTag.root)}>prod</span>
				)}
				{!environment.autoDeploy && (
					<span {...stylex.props(manualTag.root)}>manual</span>
				)}
				<ChevronDown
					size={13}
					{...stylex.props([styles.chevron, open && styles.openChevron])}
				/>
			</button>

			{open && (
				<div {...stylex.props(styles.menu)} role="menu">
					<p {...stylex.props(styles.menuLabel)}>Environments</p>
					<div {...stylex.props(styles.environmentList)}>
						{environments.map((entry) => {
							const active = entry.id === environment.id;
							if (renamingId === entry.id) {
								return (
									<form
										key={entry.id}
										{...stylex.props(styles.renameForm)}
										onSubmit={(event) => {
											event.preventDefault();
											void commitRename(entry);
										}}
									>
										<input
											ref={renameInputRef}
											{...stylex.props(styles.renameInput)}
											value={renameDraft}
											disabled={busy}
											aria-label={`Rename ${entry.name}`}
											onChange={(event) => setRenameDraft(event.target.value)}
											onKeyDown={(event) => {
												if (event.key === "Escape") {
													event.preventDefault();
													event.stopPropagation();
													setRenamingId(null);
													setError(undefined);
												}
											}}
											onBlur={() => void commitRename(entry)}
										/>
									</form>
								);
							}
							return (
								<div
									{...stylex.props([
										styles.environmentRow,
										stylex.defaultMarker(),
									])}
									key={entry.id}
								>
									<button
										type="button"
										{...stylex.props([
											styles.selectButton,
											active && styles.selectedEnvironment,
										])}
										role="menuitem"
										onClick={() => selectEnvironment(entry.id)}
									>
										<span {...stylex.props(styles.selectionIndicator)}>
											{active && <Check size={13} />}
										</span>
										<span {...stylex.props(styles.environmentName)}>
											{entry.name}
										</span>
										{entry.isProduction && (
											<span {...stylex.props(envTag.root)}>prod</span>
										)}
										{!entry.autoDeploy && (
											<span {...stylex.props(manualTag.root)}>manual</span>
										)}
									</button>
									<button
										type="button"
										{...stylex.props(styles.actionsButton)}
										aria-label={`Environment actions for ${entry.name}`}
										aria-haspopup="menu"
										aria-expanded={menuFor?.environmentId === entry.id}
										onClick={(event) => openRowMenu(entry, event.currentTarget)}
									>
										<MoreHorizontal size={14} />
									</button>
									{menuFor?.environmentId === entry.id && (
										<RowMenu anchor={menuFor} menuRef={rowMenuRef}>
											<button
												type="button"
												role="menuitem"
												{...stylex.props(rowMenuItem.root)}
												onClick={() => startRename(entry)}
											>
												Rename
											</button>
											<button
												type="button"
												role="menuitemcheckbox"
												aria-checked={entry.autoDeploy}
												{...stylex.props(rowMenuItem.root)}
												disabled={busy}
												onClick={() => void toggleAutoDeploy(entry)}
											>
												{entry.autoDeploy
													? "Turn auto-deploy off"
													: "Turn auto-deploy on"}
											</button>
											<button
												type="button"
												role="menuitem"
												{...stylex.props([
													rowMenuItem.root,
													styles.deleteButton,
												])}
												disabled={environments.length === 1}
												title={
													environments.length === 1
														? "A project keeps at least one environment. Delete the project instead."
														: undefined
												}
												onClick={() => {
													setMenuFor(null);
													setError(undefined);
													setDeleting(entry);
												}}
											>
												Delete
											</button>
										</RowMenu>
									)}
								</div>
							);
						})}
					</div>

					{error && !deleting && (
						<p {...stylex.props(styles.errorMessage)}>{error}</p>
					)}

					<button
						type="button"
						{...stylex.props(styles.createButton)}
						onClick={() => {
							closeMenus();
							onCreateEnvironment();
						}}
					>
						<Plus size={14} />
						New Environment
					</button>
				</div>
			)}

			{deleting && (
				<DeleteResourceDialog
					title={
						deleting.isProduction
							? "Delete Production Environment"
							: "Delete Environment"
					}
					name={deleting.name}
					recovery="restorable"
					requireName={deleting.isProduction}
					loadPreview={() =>
						fetchDeletionPreview({
							data: { kind: "environment", id: deleting.id },
						})
					}
					busy={busy}
					error={error}
					description={
						<>
							You are{" "}
							<span {...stylex.props(styles.deleteButton)}>deleting</span> the
							{deleting.isProduction ? " production" : ""} environment{" "}
							<strong>{deleting.name}</strong>. Its services stop serving and
							its domains stop routing.
						</>
					}
					onCancel={() => {
						if (busy) return;
						setDeleting(null);
						setError(undefined);
					}}
					onConfirm={(confirmationName) => void confirmDelete(confirmationName)}
				/>
			)}
		</div>
	);
}

function RowMenu({
	anchor,
	menuRef,
	children,
}: {
	anchor: RowMenuAnchor;
	menuRef: RefObject<HTMLDivElement | null>;
	children: ReactNode;
}) {
	const menu = (
		<div
			ref={menuRef}
			{...stylex.props(
				styles.rowMenu,
				styles.menuPosition(anchor.top, anchor.right),
			)}
			role="menu"
		>
			{children}
		</div>
	);
	if (typeof document === "undefined") return menu;
	return createPortal(menu, document.body);
}

const styles = stylex.create({
	menuPosition: (top: number, right: number) => ({ top, right }),
	root: { position: "relative" },
	trigger: {
		display: "inline-flex",
		maxWidth: "260px",
		cursor: "pointer",
		alignItems: "center",
		gap: "0.375rem",
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.line },
		},
		paddingInline: space.sm,
		paddingBlock: space.xs,
		fontFamily: fonts.sans,
		fontSize: "13px",
		fontWeight: "600",
		color: colors.ink,
		transitionProperty: "background-color,border-color",
		transitionTimingFunction: "cubic-bezier(0, 0, 0.2, 1)",
		transitionDuration: motion.fast,
		backgroundColor: {
			default: null,
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
	},
	openTrigger: {
		borderColor: colors.line,
		backgroundColor: colors.surfaceHover,
	},
	environmentName: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	chevron: { flexShrink: "0", color: colors.muted },
	openChevron: { rotate: "180deg" },
	menu: {
		position: "absolute",
		top: "calc(100% + 8px)",
		left: "0rem",
		zIndex: "60",
		minWidth: "260px",
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
	environmentList: {
		display: "flex",
		maxHeight: "20rem",
		flexDirection: "column",
		overflowY: "auto",
	},
	renameForm: { paddingInline: space.sm, paddingBlock: space.xs },
	renameInput: {
		width: "100%",
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.accent,
		backgroundColor: colors.canvas,
		paddingInline: space.sm,
		paddingBlock: "0.375rem",
		fontFamily: fonts.sans,
		fontSize: "13px",
		color: colors.ink,
		boxShadow: `0 0 0 2px ${colors.accentDim}`,
		outlineStyle: "none",
	},
	environmentRow: {
		position: "relative",
		display: "flex",
		alignItems: "center",
		backgroundColor: {
			default: null,
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
	},
	selectButton: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		cursor: "pointer",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		paddingBlock: space.sm,
		paddingRight: space.xs,
		paddingLeft: "0.625rem",
		textAlign: "left",
		fontFamily: fonts.sans,
		fontSize: "13px",
		color: colors.ink,
	},
	selectedEnvironment: { fontWeight: "600" },
	selectionIndicator: {
		display: "inline-flex",
		width: "0.875rem",
		flexShrink: "0",
		alignItems: "center",
		justifyContent: "center",
		color: colors.accent,
	},
	actionsButton: {
		marginRight: "0.375rem",
		display: "inline-flex",
		width: "1.75rem",
		height: "1.75rem",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: {
			default: "transparent",
			":hover": {
				default: null,
				"@media (hover: hover)": colors.surfaceRaised,
			},
			':is([aria-expanded="true"])': colors.surfaceRaised,
		},
		color: {
			default: colors.dim,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
			':is([aria-expanded="true"])': colors.ink,
		},
		opacity: {
			default: "0%",
			[stylex.when.ancestor(":hover")]: {
				default: null,
				"@media (hover: hover)": "100%",
			},
			':is([aria-expanded="true"])': "100%",
		},
		transitionProperty: "opacity,color",
		transitionTimingFunction: "cubic-bezier(0, 0, 0.2, 1)",
		transitionDuration: motion.fast,
	},
	deleteButton: { color: colors.failed },
	errorMessage: {
		marginInline: "0.625rem",
		marginBlock: space.xs,
		fontSize: "11px",
		color: colors.failed,
	},
	createButton: {
		marginTop: "0.375rem",
		display: "flex",
		width: "100%",
		cursor: "pointer",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "0px",
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: colors.line,
		backgroundColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.accentDim },
		},
		paddingInline: space.md,
		paddingBlock: "0.625rem",
		textAlign: "left",
		fontFamily: fonts.sans,
		fontSize: "13px",
		fontWeight: "600",
		color: colors.accent,
	},
	rowMenu: {
		position: "fixed",
		zIndex: "80",
		display: "flex",
		minWidth: "132px",
		flexDirection: "column",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: colors.surfaceRaised,
		paddingBlock: space.xs,
		boxShadow: "0 12px 32px rgba(0,0,0,0.5)",
	},
});
