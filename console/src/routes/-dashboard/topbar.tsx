import {
	AlertCircle,
	Layers,
	LogOut,
	RefreshCw,
	Server,
	Zap,
} from "lucide-react";
import { cn } from "#/lib/cn";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import { btnGhost } from "#/lib/ui-classes";

import { DeployButton } from "./deploy-button";
import { EnvironmentSwitcher } from "./environment-switcher";

export function Topbar({
	state,
	onNewService,
	onPreloadNewService,
	onRefresh,
	onNewEnvironment,
	onEnvironmentsChanged,
	onNavigateEnvironment,
}: {
	state: DashboardHomeState;
	onNewService: () => void;
	onPreloadNewService?: () => void;
	onRefresh: () => void;
	onNewEnvironment: () => void;
	onEnvironmentsChanged: () => void;
	onNavigateEnvironment: (environmentId: string | null) => void;
}) {
	return (
		<div className="fixed inset-x-0 top-0 z-40 flex h-header items-center gap-3 border-b border-line bg-surface px-4">
			<div className="mr-2 flex items-center gap-2.5">
				<Zap size={15} className="text-accent" />
				<span className="font-condensed text-base font-bold uppercase tracking-[0.14em] text-ink">
					mesh
				</span>
			</div>

			{state.project && (
				<div className="flex min-w-0 items-center gap-2">
					<span className="inline-flex items-center gap-1.5 whitespace-nowrap rounded-[1px] border border-line bg-surface-raised px-2 py-[3px] font-condensed text-[11px] font-semibold uppercase tracking-[0.06em] text-muted">
						<Layers size={12} />
						{state.project.name}
					</span>
					{state.environment && (
						<>
							<span className="text-sm text-dim">/</span>
							<EnvironmentSwitcher
								state={state}
								onCreateEnvironment={onNewEnvironment}
								onChanged={onEnvironmentsChanged}
								onNavigateEnvironment={onNavigateEnvironment}
							/>
						</>
					)}
				</div>
			)}

			{state.services.length > 0 && (
				<span className="font-mono text-[11px] tracking-[0.03em] text-dim">
					{state.services.length}{" "}
					{state.services.length === 1 ? "service" : "services"}
				</span>
			)}

			{!state.controlPlaneReachable && (
				<span className="flex items-center gap-1 text-[11px] text-failed">
					<AlertCircle size={11} /> Control plane offline
				</span>
			)}

			<div className="flex-1" />

			{state.canManageFleet && (
				<a
					href="/fleet"
					className={cn(btnGhost, "gap-1 text-xs")}
					title="Agent fleet"
				>
					<Server size={13} /> Fleet
				</a>
			)}

			<button type="button" className={btnGhost} onClick={onRefresh}>
				<RefreshCw size={13} />
			</button>

			<DeployButton
				state={state}
				onNewService={onNewService}
				onPreload={onPreloadNewService}
			/>

			<a
				href="/logout"
				className={cn(btnGhost, "gap-1 text-xs")}
				title="Sign out"
			>
				<LogOut size={13} />
			</a>
		</div>
	);
}
