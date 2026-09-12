import { Loader2, Server, Terminal } from "lucide-react";
import type { MouseEvent as ReactMouseEvent } from "react";
import { cn } from "#/lib/cn";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { badgeClass, statusDotClass } from "#/lib/ui-classes";

import { NODE_H, NODE_W } from "./layout";
import { serviceHealth, shortSha } from "./service-utils";
import { usePulseDelay } from "./use-pulse-delay";

export function ServiceNode({
	service,
	pos,
	selected,
	pending = false,
	onMouseDown,
	onSelect,
}: {
	service: DashboardServiceRecord;
	pos: { x: number; y: number };
	selected: boolean;
	pending?: boolean;
	onMouseDown: (event: ReactMouseEvent<HTMLElement>) => void;
	onSelect: () => void;
}) {
	const health = serviceHealth(service);
	const source = service.spec?.source;
	const repo = source?.repositorySelector ?? "";
	const repoShort = repo.split("/").pop() ?? repo;
	const unappliedCount =
		service.unappliedChangeCount ?? (service.pendingChanges ? 1 : 0);
	const pulseDelay = usePulseDelay(health === "building");

	return (
		<button
			type="button"
			data-service-node=""
			className={cn(
				"absolute w-[220px] cursor-pointer overflow-hidden rounded-sm border border-line bg-surface-raised p-0 text-left text-inherit select-none transition-[border-color,box-shadow] duration-100",
				selected
					? "border-accent shadow-[3px_3px_0_rgba(0,0,0,0.6),0_0_0_1px_var(--color-accent)]"
					: "hover:border-line-bright hover:shadow-[3px_3px_0_rgba(0,0,0,0.5)]",
			)}
			style={{
				left: pos.x,
				top: pos.y,
				width: NODE_W,
				height: NODE_H,
			}}
			aria-pressed={selected}
			onMouseDown={onMouseDown}
			onClick={onSelect}
		>
			<div className="flex items-center gap-2 border-b border-line px-3.5 pt-2.5 pb-2">
				<span
					className={statusDotClass(health)}
					style={
						health === "building" ? { animationDelay: pulseDelay } : undefined
					}
				/>
				<span className="flex-1 overflow-hidden font-condensed text-sm font-bold tracking-[0.03em] text-ellipsis whitespace-nowrap text-ink">
					{service.name}
				</span>
			</div>

			<div className="flex flex-col gap-[5px] px-3.5 pt-2 pb-2.5">
				{pending && (
					<div className="flex items-center gap-[5px] font-mono text-[11px] text-building">
						<Loader2 size={10} className="shrink-0 animate-spin" />
						Creating service…
					</div>
				)}
				{repoShort && (
					<div className="flex items-center gap-[5px] overflow-hidden font-mono text-[11px] text-ellipsis whitespace-nowrap text-muted">
						<Server size={10} className="shrink-0" />
						{repoShort}
					</div>
				)}

				{source?.trackedRef && (
					<div className="flex items-center gap-[5px] font-mono text-[11px] text-dim">
						<Terminal size={10} className="shrink-0" />
						{source.trackedRef}
					</div>
				)}

				<div className="mt-0.5 flex items-center justify-between gap-1.5">
					<div className="flex items-center gap-[5px]">
						{unappliedCount > 0 && (
							<span className={badgeClass("edited")}>
								{unappliedCount} {unappliedCount === 1 ? "change" : "changes"}
							</span>
						)}
					</div>

					{service.latestBuild?.commitSha && (
						<span className="font-mono text-[10px] text-dim">
							{shortSha(service.latestBuild.commitSha)}
						</span>
					)}
				</div>
			</div>
		</button>
	);
}
