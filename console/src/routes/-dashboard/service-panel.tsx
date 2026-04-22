import { Activity, Globe, Layers, RefreshCw, Settings, X } from "lucide-react";

import type {
	DashboardHomeState,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { PanelDeployments } from "./panel-deployments";
import { PanelDomains } from "./panel-domains";
import { PanelOverview } from "./panel-overview";
import { PanelSettings } from "./panel-settings";
import { healthLabel, serviceHealth } from "./service-utils";
import type { DashboardTab } from "./types";

const tabs = [
	{ id: "overview", label: "Overview", icon: <Activity size={11} /> },
	{ id: "deployments", label: "Deployments", icon: <Layers size={11} /> },
	{ id: "settings", label: "Settings", icon: <Settings size={11} /> },
	{ id: "domains", label: "Domains", icon: <Globe size={11} /> },
] as const;

export function ServicePanel({
	service,
	status,
	statusLoading,
	project,
	state,
	activeTab,
	onTabChange,
	onClose,
	onRefresh,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	statusLoading: boolean;
	project: DashboardProject | undefined;
	state: DashboardHomeState;
	activeTab: DashboardTab;
	onTabChange: (tab: DashboardTab) => void;
	onClose: () => void;
	onRefresh: () => void;
}) {
	const health = serviceHealth(service);

	return (
		<>
			<div style={{ padding: "14px 16px 0", flexShrink: 0 }}>
				<div
					style={{
						display: "flex",
						alignItems: "center",
						gap: 10,
						marginBottom: 12,
					}}
				>
					<span className={`status-dot ${health}`} style={{ flexShrink: 0 }} />
					<span
						style={{
							fontSize: 15,
							fontWeight: 700,
							color: "var(--text)",
							flex: 1,
							overflow: "hidden",
							textOverflow: "ellipsis",
							whiteSpace: "nowrap",
						}}
					>
						{service.name}
					</span>
					<span className={`badge ${health}`} style={{ flexShrink: 0 }}>
						{healthLabel(health)}
					</span>
					<button
						type="button"
						className="btn-ghost"
						onClick={onRefresh}
						style={{ padding: "4px 6px" }}
					>
						<RefreshCw size={13} />
					</button>
					<button
						type="button"
						className="btn-ghost"
						onClick={onClose}
						style={{ padding: "4px 6px" }}
					>
						<X size={14} />
					</button>
				</div>
			</div>

			<div className="tab-bar">
				{tabs.map((tab) => (
					<button
						key={tab.id}
						type="button"
						className={`tab-item ${activeTab === tab.id ? "active" : ""}`}
						onClick={() => onTabChange(tab.id)}
					>
						{tab.label}
					</button>
				))}
			</div>

			<div style={{ flex: 1, overflowY: "auto", padding: "16px" }}>
				{activeTab === "overview" && (
					<PanelOverview
						service={service}
						status={status}
						loading={statusLoading}
						state={state}
					/>
				)}
				{activeTab === "deployments" && (
					<PanelDeployments service={service} status={status} />
				)}
				{activeTab === "settings" && project && (
					<PanelSettings
						service={service}
						project={project}
						state={state}
						onSaved={onRefresh}
					/>
				)}
				{activeTab === "domains" && project && (
					<PanelDomains service={service} project={project} state={state} />
				)}
			</div>
		</>
	);
}
