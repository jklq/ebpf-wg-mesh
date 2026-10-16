import * as stylex from "@stylexjs/stylex";
import { RefreshCw, X } from "lucide-react";
import { lazy, Suspense, useCallback, useEffect, useId, useState } from "react";
import { Button } from "#/components/ui/button";
import { TabList } from "#/components/ui/tabs";
import { PanelDeployments } from "#/features/dashboard/service-panel/deployments/panel-deployments";
import { EditableServiceHeaderName } from "#/features/dashboard/service-panel/editable-service-name";
import {
	TabSkeleton,
	tabContentStyles,
} from "#/features/dashboard/service-panel/panel-skeleton";
import { servicePanelTabs } from "#/features/dashboard/service-panel/panel-tabs";
import { ServiceStatusBadge } from "#/features/dashboard/service-panel/service-status-badge";
import type { DashboardTab } from "#/features/dashboard/shared/types";
import type {
	DashboardHomeState,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";
import { colors, sizes, space } from "#/styles/tokens.stylex";

const loadVariables = () =>
	import("#/features/dashboard/service-panel/variables/panel-variables");
const loadDomains = () =>
	import("#/features/dashboard/service-panel/domains/panel-domains");
const loadSettings = () =>
	import("#/features/dashboard/service-panel/settings/panel-settings");
const loadUsage = () =>
	import("#/features/dashboard/service-panel/usage/panel-usage");
const VariablesPanel = lazy(() =>
	loadVariables().then((module) => ({ default: module.PanelVariables })),
);
const DomainsPanel = lazy(() =>
	loadDomains().then((module) => ({ default: module.PanelDomains })),
);
const SettingsPanel = lazy(() =>
	loadSettings().then((module) => ({ default: module.PanelSettings })),
);
const UsagePanel = lazy(() =>
	loadUsage().then((module) => ({ default: module.PanelUsage })),
);

export function ServicePanel({
	service,
	status,
	project,
	state,
	activeTab,
	onTabChange,
	onClose,
	onRefresh,
	onServiceUpdated,
	onServiceDeleted,
	onSpecSaveStateChange,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
	state: DashboardHomeState;
	activeTab: DashboardTab;
	onTabChange: (tab: DashboardTab) => void;
	onClose: () => void;
	onRefresh: () => void;
	onServiceUpdated: (service: DashboardServiceRecord) => void;
	onServiceDeleted: (serviceId: string) => void;
	onSpecSaveStateChange?: (key: string, saving: boolean) => void;
}) {
	const currentService = service;
	const tabsId = useId();
	const panelId = `${tabsId}-panel`;
	const [seedVariableKey, setSeedVariableKey] = useState<string>();
	const reportVariablesSaving = useCallback(
		(saving: boolean) => onSpecSaveStateChange?.("variables", saving),
		[onSpecSaveStateChange],
	);

	useEffect(() => {
		const preload = () => {
			void Promise.all([
				loadVariables(),
				loadDomains(),
				loadSettings(),
				loadUsage(),
			]).catch(() => undefined);
		};
		if (typeof window.requestIdleCallback === "function") {
			const id = window.requestIdleCallback(preload);
			return () => window.cancelIdleCallback(id);
		}
		const id = window.setTimeout(preload, 0);
		return () => window.clearTimeout(id);
	}, []);

	return (
		<>
			<div {...stylex.props(styles.header)}>
				<EditableServiceHeaderName
					service={currentService}
					project={project}
					onSaved={onServiceUpdated}
				/>
				<span {...stylex.props(styles.headerSpacer)} />
				<ServiceStatusBadge service={currentService} />
				<Button
					type="button"
					variant="panelIcon"
					onClick={onRefresh}
					title="Refresh service"
				>
					<RefreshCw size={13} />
				</Button>
				<Button
					type="button"
					variant="panelIcon"
					onClick={onClose}
					title="Close service panel"
				>
					<X size={14} />
				</Button>
			</div>

			<TabList
				items={servicePanelTabs}
				selected={activeTab}
				onSelect={onTabChange}
				label="Service"
				id={tabsId}
				panelId={panelId}
			/>

			<div
				role="tabpanel"
				id={panelId}
				aria-labelledby={`${tabsId}-${activeTab}`}
				{...stylex.props(styles.body)}
			>
				<Suspense
					fallback={<TabSkeleton tab={activeTab} service={currentService} />}
				>
					{activeTab === "deployments" && (
						<PanelDeployments
							key={service.id}
							service={service}
							status={status}
							project={project}
							domains={state.domainBindings}
							autoDeploy={state.environment?.autoDeploy ?? true}
							onOpenVariables={(key) => {
								setSeedVariableKey(key);
								onTabChange("variables");
							}}
							onRedeployed={(next) => {
								if (!next.service)
									throw new Error("Service status is missing its service");
								onServiceUpdated(next.service);
								onRefresh();
							}}
						/>
					)}
					{activeTab === "variables" && project && (
						<div {...stylex.props(tabContentStyles.content)}>
							<VariablesPanel
								service={service}
								seedKey={seedVariableKey}
								onSaved={onServiceUpdated}
								onSavingChange={reportVariablesSaving}
							/>
						</div>
					)}
					{activeTab === "settings" && project && (
						<div {...stylex.props(tabContentStyles.content)}>
							<SettingsPanel
								service={currentService}
								state={state}
								onSaved={onServiceUpdated}
								onDeleted={onServiceDeleted}
								onSavingChange={onSpecSaveStateChange}
							/>
						</div>
					)}
					{activeTab === "domains" && project && (
						<div {...stylex.props(tabContentStyles.content)}>
							<DomainsPanel service={service} state={state} status={status} />
						</div>
					)}
					{activeTab === "usage" && project && (
						<div {...stylex.props(tabContentStyles.content)}>
							<UsagePanel
								key={service.id}
								service={service}
								project={project}
							/>
						</div>
					)}
				</Suspense>
			</div>
		</>
	);
}

const styles = stylex.create({
	header: {
		display: "flex",
		height: sizes.header,
		flexShrink: "0",
		alignItems: "center",
		gap: "0.375rem",
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		backgroundImage:
			"linear-gradient(180deg,rgba(255,255,255,0.02),transparent)",
		backgroundColor: "rgba(24,23,21,0.92)",
		paddingInline: space.lg,
		paddingBlock: space.sm,
	},
	headerSpacer: { flex: "1" },
	body: {
		position: "relative",
		minHeight: "0rem",
		flex: "1",
		overflow: "hidden",
	},
});
