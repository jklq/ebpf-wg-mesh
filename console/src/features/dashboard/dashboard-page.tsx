import * as stylex from "@stylexjs/stylex";
import { lazy, Suspense } from "react";
import { DashboardCanvasStage } from "#/features/dashboard/canvas/dashboard-canvas";
import { UnappliedChangesDialog } from "#/features/dashboard/changes/dashboard-unapplied";
import { WorkspaceReleasePrompt } from "#/features/dashboard/changes/workspace-release-prompt";
import { EnvironmentDialog } from "#/features/dashboard/navigation/environment-dialog";
import { Topbar } from "#/features/dashboard/navigation/topbar";
import { ServicePanelFallback } from "#/features/dashboard/service-panel/panel-fallback";
import { PendingServicePanel } from "#/features/dashboard/service-panel/pending-service-panel";
import { NewServiceModal } from "#/features/dashboard/services/new-service-modal";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import { colors, motion, sizes } from "#/styles/tokens.stylex";

const ServicePanel = lazy(() =>
	import("#/features/dashboard/service-panel/service-panel").then((module) => ({
		default: module.ServicePanel,
	})),
);

import { useDashboardController } from "./use-dashboard-controller";
export function DashboardPage({
	state,
	urlSelectedServiceId,
}: {
	state: DashboardHomeState;
	urlSelectedServiceId?: string | null;
}) {
	const { homeState, canvas, navigation, selection, creation, release } =
		useDashboardController({ state, urlSelectedServiceId });

	return (
		<div {...stylex.props(styles.page)}>
			<Topbar
				state={homeState}
				onNewService={creation.open}
				onPreloadNewService={creation.preload}
				onRefresh={navigation.refresh}
				onNewEnvironment={() => navigation.openEnvironmentDialog()}
				onEnvironmentsChanged={navigation.refresh}
				onNavigateEnvironment={navigation.navigateEnvironment}
			/>
			{navigation.environmentDialogOpen && (
				<EnvironmentDialog
					state={homeState}
					onClose={() => navigation.closeEnvironmentDialog()}
					onCreated={(environmentId) => {
						navigation.closeEnvironmentDialog();
						navigation.navigateEnvironment(environmentId);
					}}
				/>
			)}
			<DashboardCanvasStage
				canvas={canvas}
				onSelectService={(serviceId) => {
					const id = canvas.selectService(serviceId);
					if (!id) return;
					selection.select(id);
				}}
				onEscape={() => {
					selection.clear();
				}}
				localState={homeState}
				showNewService={creation.isOpen}
				onAddService={creation.open}
				onPreloadAdd={creation.preload}
			>
				{release.showPrompt && (
					<WorkspaceReleasePrompt
						release={release}
						panelOpen={Boolean(selection.service || selection.pending)}
					/>
				)}
			</DashboardCanvasStage>

			<div
				{...stylex.props([
					styles.servicePanelShell,
					(selection.service || selection.pending) && styles.servicePanelOpen,
				])}
			>
				<div {...stylex.props(styles.panelGrain)} />
				{selection.pending ? (
					<PendingServicePanel
						service={selection.pending}
						onClose={selection.clear}
					/>
				) : selection.service ? (
					<Suspense
						fallback={
							<ServicePanelFallback
								service={selection.service}
								onClose={selection.clear}
								onRefresh={navigation.refresh}
							/>
						}
					>
						<ServicePanel
							service={selection.service}
							status={selection.status}
							project={selection.project}
							state={homeState}
							activeTab={selection.tab}
							onTabChange={selection.changeTab}
							onClose={() => {
								selection.clear();
							}}
							onRefresh={navigation.refresh}
							onServiceUpdated={selection.update}
							onServiceDeleted={selection.delete}
							onSpecSaveStateChange={selection.reportSaving}
						/>
					</Suspense>
				) : null}
			</div>

			{creation.isOpen && (
				<NewServiceModal
					state={homeState}
					catalogLoading={creation.catalogLoading}
					initialError={creation.error}
					onClose={creation.close}
					onCreated={creation.onCreated}
					onCreating={creation.onCreating}
					onCreateFailed={creation.onFailed}
				/>
			)}

			{release.showChangeDetails && (
				<UnappliedChangesDialog
					services={release.dirtyServices}
					totalChanges={release.totalUnappliedChanges}
					applyingChanges={release.applyingChangeCount}
					deployableChanges={release.deployableUnappliedChanges}
					affectedServices={release.dirtyServices.length}
					deploying={release.deployingChanges}
					creationPending={release.pendingCreationCount > 0}
					deployError={release.deployError}
					discardingChangeId={release.discardingChangeId}
					onClose={() => release.setShowChangeDetails(false)}
					onDeploy={release.handleDeployChanges}
					onDiscardChange={release.handleDiscardChange}
					onDiscardService={release.handleDiscardServiceChanges}
				/>
			)}
		</div>
	);
}

const styles = stylex.create({
	page: {
		display: "flex",
		height: "100dvh",
		flexDirection: "column",
		overflow: "hidden",
	},
	servicePanelShell: {
		position: "fixed",
		top: "0rem",
		right: "0rem",
		zIndex: "50",
		display: "flex",
		height: "100vh",
		width: { default: sizes.sidePanel, "@media (width < 900px)": "100vw" },
		maxWidth: "100%",
		translate: "100% 0",
		flexDirection: "column",
		overflow: "hidden",
		borderLeftStyle: "solid",
		borderLeftWidth: "1px",
		borderColor: colors.line,
		backgroundImage:
			"radial-gradient(920px 420px at 100% -40px,rgba(226,138,36,0.08),transparent 58%),linear-gradient(180deg,#1d1a16 0%,#141210 72%)",
		transitionProperty: "transform, translate, scale, rotate",
		transitionTimingFunction: "cubic-bezier(0, 0, 0.2, 1)",
		transitionDuration: motion.normal,
	},
	servicePanelOpen: { translate: "0rem 0" },
	panelGrain: {
		pointerEvents: "none",
		position: "absolute",
		inset: "0",
		opacity: "0.045",
		mixBlendMode: "overlay",
		backgroundImage:
			"url(\"data:image/svg+xml,%3Csvg viewBox='0 0 160 160' xmlns='http://www.w3.org/2000/svg'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='.85' numOctaves='4' stitchTiles='stitch'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23n)'/%3E%3C/svg%3E\")",
	},
});
