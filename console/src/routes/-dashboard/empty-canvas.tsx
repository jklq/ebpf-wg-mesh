import { Layers } from "lucide-react";

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
		<div className="empty-canvas">
			<div
				style={{
					width: 56,
					height: 56,
					borderRadius: 2,
					background: "var(--surface-raised)",
					border: "1px solid var(--border)",
					display: "flex",
					alignItems: "center",
					justifyContent: "center",
				}}
			>
				<Layers size={24} color="var(--text-dim)" />
			</div>
			<div style={{ textAlign: "center" }}>
				<p
					style={{
						margin: "0 0 4px",
						fontSize: 15,
						fontWeight: 700,
						letterSpacing: "0.08em",
						textTransform: "uppercase",
						fontFamily: "'Barlow Condensed', sans-serif",
						color: "var(--text)",
					}}
				>
					No services
				</p>
				<p style={{ margin: 0, fontSize: 12, color: "var(--text-muted)" }}>
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
