import { Check, ChevronDown, MoreHorizontal, Plus } from "lucide-react";
import type { ReactNode, RefObject } from "react";
import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import { cn } from "#/lib/cn";
import type {
	DashboardEnvironment,
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";

import { DeleteDialog } from "./delete-dialog";
import {
	doDeleteResource,
	doRenameEnvironment,
	doUpdateEnvironmentAutoDeploy,
	fetchDeletionPreview,
} from "./server-fns";
import { formatError } from "./service-utils";

type RowMenuAnchor = { environmentId: string; top: number; right: number };

const envTag =
	"rounded-[1px] border border-accent-glow bg-accent-dim px-[5px] py-px font-condensed text-[9px] font-bold uppercase tracking-[0.1em] text-accent";
const manualTag =
	"rounded-[1px] border border-line bg-surface-raised px-[5px] py-px font-condensed text-[9px] font-bold uppercase tracking-[0.1em] text-muted";
const rowMenuItem =
	"cursor-pointer border-0 bg-transparent px-3 py-[7px] text-left font-sans text-xs text-ink enabled:hover:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-35";

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
		<div className="relative" ref={rootRef}>
			<button
				type="button"
				className={cn(
					"inline-flex max-w-[260px] cursor-pointer items-center gap-1.5 rounded-[1px] border border-transparent px-2 py-1 font-sans text-[13px] font-semibold text-ink transition-[background-color,border-color] duration-100 ease-out hover:border-line hover:bg-surface-hover",
					open && "border-line bg-surface-hover",
				)}
				aria-haspopup="menu"
				aria-expanded={open}
				aria-label="Environment"
				onClick={() => (open ? closeMenus() : setOpen(true))}
			>
				<span className="truncate">{environment.name}</span>
				{environment.isProduction && <span className={envTag}>prod</span>}
				{!environment.autoDeploy && <span className={manualTag}>manual</span>}
				<ChevronDown
					size={13}
					className={cn("shrink-0 text-muted", open && "rotate-180")}
				/>
			</button>

			{open && (
				<div
					className="absolute top-[calc(100%+8px)] left-0 z-[60] min-w-[260px] animate-slide-down rounded-sm border border-line-bright bg-surface py-1.5 shadow-[0_18px_50px_rgba(0,0,0,0.55)]"
					role="menu"
				>
					<p className="m-0 px-3 pt-1.5 pb-2 font-condensed text-[10px] font-bold tracking-[0.1em] text-muted uppercase">
						Environments
					</p>
					<div className="flex max-h-80 flex-col overflow-y-auto">
						{environments.map((entry) => {
							const active = entry.id === environment.id;
							if (renamingId === entry.id) {
								return (
									<form
										key={entry.id}
										className="px-2 py-1"
										onSubmit={(event) => {
											event.preventDefault();
											void commitRename(entry);
										}}
									>
										<input
											ref={renameInputRef}
											className="w-full rounded-[1px] border border-accent bg-canvas px-2 py-1.5 font-sans text-[13px] text-ink shadow-[0_0_0_2px_var(--color-accent-dim)] outline-none"
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
									className="group relative flex items-center hover:bg-surface-hover"
									key={entry.id}
								>
									<button
										type="button"
										className={cn(
											"flex min-w-0 flex-1 cursor-pointer items-center gap-2 border-0 bg-transparent py-2 pr-1 pl-2.5 text-left font-sans text-[13px] text-ink",
											active && "font-semibold",
										)}
										role="menuitem"
										onClick={() => selectEnvironment(entry.id)}
									>
										<span className="inline-flex w-3.5 shrink-0 items-center justify-center text-accent">
											{active && <Check size={13} />}
										</span>
										<span className="truncate">{entry.name}</span>
										{entry.isProduction && <span className={envTag}>prod</span>}
										{!entry.autoDeploy && (
											<span className={manualTag}>manual</span>
										)}
									</button>
									<button
										type="button"
										className="mr-1.5 inline-flex size-7 cursor-pointer items-center justify-center rounded-[1px] border-0 bg-transparent text-dim opacity-0 transition-[opacity,color] duration-100 ease-out group-hover:opacity-100 hover:bg-surface-raised hover:text-ink aria-expanded:bg-surface-raised aria-expanded:text-ink aria-expanded:opacity-100"
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
												className={rowMenuItem}
												onClick={() => startRename(entry)}
											>
												Rename
											</button>
											<button
												type="button"
												role="menuitemcheckbox"
												aria-checked={entry.autoDeploy}
												className={rowMenuItem}
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
												className={cn(rowMenuItem, "text-failed")}
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
						<p className="mx-2.5 my-1 text-[11px] text-failed">{error}</p>
					)}

					<button
						type="button"
						className="mt-1.5 flex w-full cursor-pointer items-center gap-2 border-0 border-t border-line bg-transparent px-3 py-2.5 text-left font-sans text-[13px] font-semibold text-accent hover:bg-accent-dim"
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
				<DeleteDialog
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
							You are <span className="text-failed">deleting</span> the
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
			className="fixed z-[80] flex min-w-[132px] flex-col rounded-sm border border-line-bright bg-surface-raised py-1 shadow-[0_12px_32px_rgba(0,0,0,0.5)]"
			role="menu"
			style={{ top: anchor.top, right: anchor.right }}
		>
			{children}
		</div>
	);
	if (typeof document === "undefined") return menu;
	return createPortal(menu, document.body);
}
