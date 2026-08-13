import { Check, ChevronDown, MoreHorizontal, Plus } from "lucide-react";
import type { ReactNode, RefObject } from "react";
import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import type {
	DashboardEnvironment,
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";

import { ConfirmDeleteDialog } from "./confirm-delete-dialog";
import { doDeleteEnvironment, doRenameEnvironment } from "./server-fns";
import { formatError } from "./service-utils";

type RowMenuAnchor = { environmentId: string; top: number; right: number };

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
			// The row menu is portalled to <body> so it can escape the scrolling
			// environment list — it is outside rootRef but still "inside" the menu.
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

	// The row menu is anchored to viewport coordinates captured when it opened,
	// so any scroll or resize invalidates its position.
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

	// The active environment is normally part of the list; keep it visible even
	// if a refresh raced the list update.
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

	const confirmDelete = async () => {
		if (!deleting) return;
		setBusy(true);
		setError(undefined);
		try {
			await doDeleteEnvironment({ data: { environmentId: deleting.id } });
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
		<div className="env-switcher" ref={rootRef}>
			<button
				type="button"
				className={`env-trigger ${open ? "open" : ""}`}
				aria-haspopup="menu"
				aria-expanded={open}
				aria-label="Environment"
				onClick={() => (open ? closeMenus() : setOpen(true))}
			>
				<span className="env-trigger-name">{environment.name}</span>
				{environment.isProduction && <span className="env-tag">prod</span>}
				<ChevronDown size={13} className="env-trigger-chevron" />
			</button>

			{open && (
				<div className="env-menu" role="menu">
					<p className="env-menu-heading">Environments</p>
					<div className="env-menu-list">
						{environments.map((entry) => {
							const active = entry.id === environment.id;
							if (renamingId === entry.id) {
								return (
									<form
										key={entry.id}
										className="env-menu-rename"
										onSubmit={(event) => {
											event.preventDefault();
											void commitRename(entry);
										}}
									>
										<input
											ref={renameInputRef}
											className="env-rename-input"
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
									className={`env-menu-row ${active ? "active" : ""}`}
									key={entry.id}
								>
									<button
										type="button"
										className="env-menu-item"
										role="menuitem"
										onClick={() => selectEnvironment(entry.id)}
									>
										<span className="env-menu-check">
											{active && <Check size={13} />}
										</span>
										<span className="env-menu-name">{entry.name}</span>
										{entry.isProduction && (
											<span className="env-tag">prod</span>
										)}
									</button>
									<button
										type="button"
										className="env-menu-more"
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
												onClick={() => startRename(entry)}
											>
												Rename
											</button>
											<button
												type="button"
												role="menuitem"
												className="danger"
												disabled={
													entry.isProduction || environments.length === 1
												}
												title={
													entry.isProduction
														? "The production environment cannot be deleted"
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

					{error && !deleting && <p className="env-menu-error">{error}</p>}

					<button
						type="button"
						className="env-menu-new"
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
				<ConfirmDeleteDialog
					title="Delete Environment"
					name={deleting.name}
					busy={busy}
					error={error}
					description={
						<>
							You are <span className="danger-word">deleting</span> the
							environment <strong>{deleting.name}</strong> and every service,
							variable, and runtime state inside it.
						</>
					}
					onCancel={() => {
						if (busy) return;
						setDeleting(null);
						setError(undefined);
					}}
					onConfirm={() => void confirmDelete()}
				/>
			)}
		</div>
	);
}

/**
 * Portalled to <body> so the row actions are never clipped by the scrolling
 * environment list they belong to — they lay out against the whole viewport.
 */
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
			className="env-row-menu"
			role="menu"
			style={{ top: anchor.top, right: anchor.right }}
		>
			{children}
		</div>
	);
	if (typeof document === "undefined") return menu;
	return createPortal(menu, document.body);
}
