import { Link } from "@tanstack/react-router";
import { Check, ChevronDown, History, Layers, Settings } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { cn } from "#/lib/cn";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

const menuLink =
	"flex items-center gap-2 px-3 py-2 font-sans text-[13px] text-ink no-underline hover:bg-surface-hover";

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
		<div className="relative" ref={rootRef}>
			<button
				type="button"
				className={cn(
					"inline-flex max-w-[220px] cursor-pointer items-center gap-1.5 whitespace-nowrap rounded-[1px] border border-line bg-surface-raised px-2 py-[3px] font-condensed text-[11px] font-semibold uppercase tracking-[0.06em] text-muted transition-colors duration-100 hover:border-line-bright hover:text-ink",
					open && "border-line-bright text-ink",
				)}
				aria-haspopup="menu"
				aria-expanded={open}
				aria-label="Project"
				onClick={() => setOpen((current) => !current)}
			>
				<Layers size={12} className="shrink-0" />
				<span className="truncate">{project.name}</span>
				<ChevronDown
					size={12}
					className={cn("shrink-0", open && "rotate-180")}
				/>
			</button>

			{open && (
				<div
					className="absolute top-[calc(100%+8px)] left-0 z-[60] min-w-[240px] animate-slide-down rounded-sm border border-line-bright bg-surface py-1.5 shadow-[0_18px_50px_rgba(0,0,0,0.55)]"
					role="menu"
				>
					<p className="m-0 px-3 pt-1.5 pb-2 font-condensed text-[10px] font-bold tracking-[0.1em] text-muted uppercase">
						Projects
					</p>
					<div className="flex max-h-72 flex-col overflow-y-auto">
						{projects.map((entry) => {
							const active = entry.id === project.id;
							return (
								<Link
									key={entry.id}
									to="/projects/$projectId"
									params={{ projectId: entry.id }}
									role="menuitem"
									className={cn(menuLink, "pl-2.5", active && "font-semibold")}
									onClick={() => setOpen(false)}
								>
									<span className="inline-flex w-3.5 shrink-0 items-center justify-center text-accent">
										{active && <Check size={13} />}
									</span>
									<span className="truncate">{entry.name}</span>
								</Link>
							);
						})}
					</div>
					<div className="mt-1.5 flex flex-col border-t border-line pt-1.5">
						<Link
							to="/projects/$projectId/settings"
							params={{ projectId: project.id }}
							role="menuitem"
							className={menuLink}
							onClick={() => setOpen(false)}
						>
							<Settings size={13} className="text-muted" />
							Project settings
						</Link>
						<Link
							to="/deleted"
							role="menuitem"
							className={menuLink}
							onClick={() => setOpen(false)}
						>
							<History size={13} className="text-muted" />
							Recently deleted
						</Link>
					</div>
				</div>
			)}
		</div>
	);
}
