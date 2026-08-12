import { AlertCircle, Layers, LogOut, RefreshCw, Zap } from "lucide-react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

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
		<div className="topbar">
			<div
				style={{
					display: "flex",
					alignItems: "center",
					gap: 10,
					marginRight: 8,
				}}
			>
				<Zap size={15} color="var(--accent)" />
				<span
					style={{
						fontSize: 16,
						fontWeight: 700,
						letterSpacing: "0.14em",
						textTransform: "uppercase",
						fontFamily: "'Barlow Condensed', sans-serif",
						color: "var(--text)",
					}}
				>
					mesh
				</span>
			</div>

			{state.project && (
				<div className="breadcrumb">
					<span className="breadcrumb-project">
						<Layers size={12} />
						{state.project.name}
					</span>
					{state.environment && (
						<>
							<span className="breadcrumb-sep">/</span>
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
				<span
					style={{
						fontSize: 11,
						color: "var(--text-dim)",
						fontFamily: "var(--font-mono)",
						letterSpacing: "0.03em",
					}}
				>
					{state.services.length}{" "}
					{state.services.length === 1 ? "service" : "services"}
				</span>
			)}

			{!state.controlPlaneReachable && (
				<span
					style={{
						fontSize: 11,
						color: "var(--failed)",
						display: "flex",
						alignItems: "center",
						gap: 4,
					}}
				>
					<AlertCircle size={11} /> Control plane offline
				</span>
			)}

			<div style={{ flex: 1 }} />

			<button type="button" className="btn-ghost" onClick={onRefresh}>
				<RefreshCw size={13} />
			</button>

			<DeployButton
				state={state}
				onNewService={onNewService}
				onPreload={onPreloadNewService}
			/>

			<a
				href="/logout"
				className="btn-ghost"
				style={{ gap: 4, fontSize: 12 }}
				title="Sign out"
			>
				<LogOut size={13} />
			</a>
		</div>
	);
}
