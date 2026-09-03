import { Layers } from "lucide-react";
import { cn } from "#/lib/cn";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

import { DeployButton } from "./deploy-button";

export function EmptyCanvas({
	state,
	onAdd,
	onPreloadAdd,
}: {
	state: DashboardHomeState;
	onAdd: () => void;
	onPreloadAdd?: () => void;
}) {
	return (
		<div
			className={cn(
				"pointer-events-none absolute inset-0 z-20 flex h-full flex-col items-center justify-center gap-4 text-muted",
				"[&_button]:pointer-events-auto [&_a]:pointer-events-auto",
			)}
		>
			<div className="flex size-14 items-center justify-center rounded-sm border border-line bg-surface-raised">
				<Layers size={24} className="text-dim" />
			</div>
			<div className="text-center">
				<p className="m-0 mb-1 font-condensed text-[15px] font-bold uppercase tracking-[0.08em] text-ink">
					No services
				</p>
				<p className="m-0 text-xs text-muted">
					Deploy your first service from a GitHub repo
				</p>
			</div>
			<DeployButton
				state={state}
				onNewService={onAdd}
				onPreload={onPreloadAdd}
				stopPropagation
			/>
		</div>
	);
}
