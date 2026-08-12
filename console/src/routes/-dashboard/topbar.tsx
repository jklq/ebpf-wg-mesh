import {
	AlertCircle,
	Layers,
	LogOut,
	RefreshCw,
	Settings,
	Zap,
} from "lucide-react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

import { DeployButton } from "./deploy-button";

export function Topbar({
	state,
	onNewService,
	onPreloadNewService,
	onRefresh,
	onManageEnvironments,
}: {
	state: DashboardHomeState;
	onNewService: () => void;
	onPreloadNewService?: () => void;
	onRefresh: () => void;
	onManageEnvironments: () => void;
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
				<div
					style={{
						display: "flex",
						alignItems: "center",
						gap: 6,
						padding: "3px 8px",
						background: "var(--surface-raised)",
						border: "1px solid var(--border)",
						borderRadius: 1,
						fontSize: 11,
						fontWeight: 600,
						letterSpacing: "0.06em",
						textTransform: "uppercase",
						fontFamily: "'Barlow Condensed', sans-serif",
						color: "var(--text-muted)",
						cursor: "default",
					}}
				>
					<Layers size={11} />
					{state.project.name}
				</div>
			)}
			{state.environment && (
				<>
					<select
						aria-label="Environment"
						value={state.environment.id}
						onChange={(event) =>
							window.location.assign(
								`/environments/${encodeURIComponent(event.target.value)}`,
							)
						}
					>
						{state.environments.map((environment) => (
							<option key={environment.id} value={environment.id}>
								{environment.name}
								{environment.isProduction ? " · Production" : ""}
							</option>
						))}
					</select>
					{state.environment.isProduction && (
						<span
							style={{
								fontSize: 10,
								color: "var(--accent)",
								textTransform: "uppercase",
							}}
						>
							Production
						</span>
					)}
					<button
						type="button"
						className="btn-ghost"
						title="Manage environments"
						onClick={onManageEnvironments}
					>
						<Settings size={12} />
					</button>
				</>
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
