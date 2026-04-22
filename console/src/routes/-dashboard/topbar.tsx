import {
	AlertCircle,
	Layers,
	LogOut,
	Plus,
	RefreshCw,
	Zap,
} from "lucide-react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

export function Topbar({
	state,
	onNewService,
	onRefresh,
}: {
	state: DashboardHomeState;
	onNewService: () => void;
	onRefresh: () => void;
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
				<Zap size={16} color="var(--accent)" />
				<span
					style={{
						fontSize: 13,
						fontWeight: 700,
						letterSpacing: "0.04em",
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
						padding: "3px 10px",
						background: "var(--surface-raised)",
						border: "1px solid var(--border)",
						borderRadius: 20,
						fontSize: 12,
						fontWeight: 600,
						color: "var(--text-muted)",
						cursor: "default",
					}}
				>
					<Layers size={11} />
					{state.project.name}
				</div>
			)}

			{state.services.length > 0 && (
				<span
					style={{
						fontSize: 11,
						color: "var(--text-dim)",
						fontFamily: "var(--font-mono)",
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

			{!state.githubAccount && state.githubLoginURL && (
				<a href={state.githubLoginURL} className="btn-secondary">
					Connect GitHub
				</a>
			)}

			<button type="button" className="btn-primary" onClick={onNewService}>
				<Plus size={13} />
				New service
			</button>

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
