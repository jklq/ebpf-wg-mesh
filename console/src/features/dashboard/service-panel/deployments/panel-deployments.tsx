import * as stylex from "@stylexjs/stylex";
import { ArrowLeft, Boxes, EyeOff, Globe2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";
import { DeploymentHistoryRow } from "#/features/dashboard/service-panel/deployments/deployment-history-row";
import {
	isInProgressDeploymentState,
	partitionDeployments,
} from "#/features/dashboard/service-panel/deployments/deployment-inline";
import { DeploymentCard } from "#/features/dashboard/service-panel/deployments/panel-deployment-cards";
import { DeploymentLogsView } from "#/features/dashboard/service-panel/deployments/panel-deployment-logs";
import {
	compareDeploymentsNewestFirst,
	deploymentSubtitle,
	deploymentTitle,
	hasActiveDeployment,
	newIdempotencyKey,
	pendingManualDeployRevision,
	reconcileCurrentDeployment,
	shouldRenderDeploymentHistoryEntry,
} from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import { shortSha } from "#/features/dashboard/shared/service-utils";
import { usePolling } from "#/hooks/use-polling";
import type {
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDomainBinding,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";
import {
	doApplyDeploymentAction,
	doReleaseEnvironment,
	fetchServiceDeployments,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { dateMillis } from "#/lib/time";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

type DeploymentLogTarget = {
	id: string;
	title: string;
	subtitle?: string;
	build: DashboardDeploymentRecord["build"];
	allocation: DashboardServiceStatus["allocation"];
	rolloutGeneration?: string;
	active: boolean;
};

export function PanelDeployments({
	service,
	status,
	project,
	domains = [],
	autoDeploy = true,
	onOpenVariables,
	onRedeployed,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
	domains?: DashboardDomainBinding[];
	autoDeploy?: boolean;
	onOpenVariables?: (key: string) => void;
	onRedeployed?: (status: DashboardServiceStatus) => void;
}) {
	const currentService = status?.service ?? service;
	const publicDomain = domains.find(
		(binding) =>
			binding.serviceId === currentService.id &&
			binding.ownershipState !== "DOMAIN_OWNERSHIP_STATE_UNVERIFIED",
	)?.hostname;
	const replicaCount = currentService.desiredReplicaCount ?? 1;
	const [deployments, setDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [deploymentsError, setDeploymentsError] = useState<string>();
	const [deploymentsLoading, setDeploymentsLoading] = useState(true);
	const [logTarget, setLogTarget] = useState<DeploymentLogTarget | null>(null);
	const [historyOpen, setHistoryOpen] = useState(false);
	const [nowMs, setNowMs] = useState(() => Date.now());
	const [deployingRevision, setDeployingRevision] = useState(false);
	const [revisionDeployError, setRevisionDeployError] = useState<string>();
	const pendingRevision = pendingManualDeployRevision(
		currentService,
		autoDeploy,
	);
	const actionKeys = useRef(new Map<string, string>());
	const deploymentActivityKey = [
		currentService.latestBuild?.buildId,
		currentService.latestBuild?.state,
		currentService.latestDeployment?.state,
		dateMillis(currentService.latestDeployment?.transitionedAt),
	].join(":");

	const loadDeployments = useCallback(async () => {
		if (!project) {
			setDeployments([]);
			setDeploymentsError(undefined);
			setDeploymentsLoading(false);
			return;
		}
		try {
			const nextDeployments = await fetchServiceDeployments({
				data: {
					serviceId: service.id,
					limit: 10,
				},
			});
			setDeployments(
				Array.isArray(nextDeployments)
					? nextDeployments.sort(compareDeploymentsNewestFirst)
					: [],
			);
			setDeploymentsError(undefined);
		} catch (cause) {
			setDeployments([]);
			setDeploymentsError(formatError(cause, "Unable to load deployments."));
		} finally {
			setDeploymentsLoading(false);
		}
	}, [project, service.id]);

	useEffect(() => {
		// A status event is the invalidation signal for this service's history.
		void deploymentActivityKey;
		void loadDeployments();
	}, [deploymentActivityKey, loadDeployments]);

	const applyAction = useCallback(
		async (
			record: DashboardDeploymentRecord,
			action: DashboardDeploymentAction,
			allocationId?: string,
		) => {
			if (
				action === "DEPLOYMENT_ACTION_ROLLBACK" ||
				action === "DEPLOYMENT_ACTION_EXACT_REDEPLOY"
			) {
				const image =
					record.artifact?.imageRef ?? record.build?.artifact?.imageRef;
				if (
					!window.confirm(
						`${action === "DEPLOYMENT_ACTION_ROLLBACK" ? "Roll back" : "Redeploy"}${image ? ` using ${image}` : " this deployment"}? Pinned secret versions will be restored.`,
					)
				)
					return;
			}
			const requestKey = `${record.id}:${action}:${allocationId ?? "all"}`;
			let idempotencyKey = actionKeys.current.get(requestKey);
			if (!idempotencyKey) {
				idempotencyKey = newIdempotencyKey();
				actionKeys.current.set(requestKey, idempotencyKey);
			}
			const next = await doApplyDeploymentAction({
				data: {
					serviceId: service.id,
					deploymentId: record.id,
					action,
					idempotencyKey,
					allocationId,
				},
			});
			actionKeys.current.delete(requestKey);
			onRedeployed?.(next);
			await loadDeployments();
		},
		[service.id, loadDeployments, onRedeployed],
	);

	const deployRevision = useCallback(async () => {
		if (deployingRevision) return;
		setDeployingRevision(true);
		setRevisionDeployError(undefined);
		try {
			const statuses = await doReleaseEnvironment({
				data: { environmentId: currentService.environmentId },
			});
			const next = statuses.find(
				(entry) => entry.service?.id === currentService.id,
			);
			if (next) onRedeployed?.(next);
			await loadDeployments();
		} catch (cause) {
			setRevisionDeployError(formatError(cause, "Unable to deploy."));
		} finally {
			setDeployingRevision(false);
		}
	}, [
		currentService.environmentId,
		currentService.id,
		deployingRevision,
		loadDeployments,
		onRedeployed,
	]);

	useEffect(() => {
		const id = window.setInterval(() => setNowMs(Date.now()), 30_000);
		return () => window.clearInterval(id);
	}, []);

	const visibleDeployments = useMemo(
		() => reconcileCurrentDeployment(deployments, currentService),
		[deployments, currentService],
	);
	const { live: liveDeployments, history: previousDeployments } =
		useMemo(() => {
			const partitioned = partitionDeployments(visibleDeployments);
			return {
				live: partitioned.live.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
				history: partitioned.history.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
			};
		}, [visibleDeployments, currentService]);

	const shouldPollDeployments = liveDeployments.some(
		(entry) =>
			isInProgressDeploymentState(entry.status?.state) ||
			hasActiveDeployment(entry.status, entry.build),
	);
	const deploymentInProgress = liveDeployments.some(
		(entry) =>
			isInProgressDeploymentState(entry.status?.state) ||
			entry.build?.state === "BUILD_STATE_QUEUED" ||
			entry.build?.state === "BUILD_STATE_RUNNING",
	);

	usePolling(loadDeployments, {
		enabled: shouldPollDeployments,
		intervalMs: 5000,
	});

	useEffect(() => {
		if (!logTarget) return;
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key !== "Escape") return;
			event.preventDefault();
			setLogTarget(null);
		};
		document.addEventListener("keydown", onKeyDown, { capture: true });
		return () =>
			document.removeEventListener("keydown", onKeyDown, { capture: true });
	}, [logTarget]);

	useEffect(() => {
		if (!project && logTarget) setLogTarget(null);
	}, [project, logTarget]);

	return (
		<div {...stylex.props(styles.panel)}>
			<div {...stylex.props(styles.summary)}>
				<div {...stylex.props(styles.serviceMetadata)}>
					<div {...stylex.props(styles.publicDomain)}>
						{publicDomain ? (
							<Globe2 size={15} {...stylex.props(styles.metadataIcon)} />
						) : (
							<EyeOff size={15} {...stylex.props(styles.metadataIcon)} />
						)}
						<span {...stylex.props(styles.hostname)}>
							{publicDomain ?? "Unexposed service"}
						</span>
					</div>
					<div {...stylex.props(styles.replicas)}>
						<Boxes size={15} {...stylex.props(styles.metadataIcon)} />
						<span {...stylex.props(styles.hostname)}>
							{replicaCount} {replicaCount === 1 ? "Replica" : "Replicas"}
						</span>
					</div>
				</div>
				{pendingRevision && (
					<output
						aria-label="Latest commit is waiting for manual deploy"
						{...stylex.props([
							styles.manualDeployNotice,
							styles.manualDeployNoticeEmphasis,
						])}
					>
						<span {...stylex.props(styles.manualDeployCopy)}>
							Auto-deploy is off —{" "}
							<span {...stylex.props(styles.pendingCommit)}>
								{shortSha(pendingRevision.commitSha)}
							</span>{" "}
							is waiting.
						</span>
						<Button
							type="button"
							variant="secondary"
							styles={[styles.deployRevisionButton]}
							disabled={deployingRevision}
							onClick={() => void deployRevision()}
						>
							{deployingRevision ? "Deploying…" : "Deploy now"}
						</Button>
					</output>
				)}
				{revisionDeployError && (
					<div
						{...stylex.props([noticeStyles.error, styles.deployRevisionError])}
					>
						{revisionDeployError}
					</div>
				)}
				<div {...stylex.props(styles.liveDeployments)}>
					{liveDeployments.map((entry) => (
						<DeploymentCard
							key={entry.id}
							service={currentService}
							record={entry}
							logsEnabled={Boolean(project)}
							allocations={status?.allocations ?? []}
							nowMs={nowMs}
							deploymentInProgress={deploymentInProgress}
							onOpenVariables={onOpenVariables}
							onAction={applyAction}
							onOpenLogs={() =>
								setLogTarget({
									id: entry.id,
									title: deploymentTitle(entry.build, entry.isCurrent),
									subtitle: deploymentSubtitle(
										entry.build,
										entry.rolloutGeneration,
										nowMs,
									),
									build: entry.build,
									allocation: status?.allocations.find(
										(allocation) =>
											allocation.desiredRolloutGeneration ===
											entry.rolloutGeneration,
									),
									rolloutGeneration: entry.rolloutGeneration,
									active: hasActiveDeployment(entry.status, entry.build),
								})
							}
						/>
					))}
				</div>

				{previousDeployments.length > 0 && (
					<section {...stylex.props(styles.history)}>
						<button
							type="button"
							{...stylex.props(styles.historyToggle)}
							aria-expanded={historyOpen}
							onClick={() => setHistoryOpen((open) => !open)}
						>
							<span {...stylex.props(styles.historyChevron)} aria-hidden>
								{historyOpen ? "▾" : "▸"}
							</span>
							History
							<span {...stylex.props(styles.historyCount)}>
								{previousDeployments.length}
							</span>
						</button>
						{historyOpen && (
							<div {...stylex.props(styles.historyRows)}>
								{previousDeployments.map((entry) => (
									<DeploymentHistoryRow
										serviceId={service.id}
										key={entry.id}
										build={entry.build}
										allocation={status?.allocations.find(
											(allocation) =>
												allocation.desiredRolloutGeneration ===
												entry.rolloutGeneration,
										)}
										status={entry.status}
										record={entry}
										logsEnabled={Boolean(project)}
										nowMs={nowMs}
										deploymentInProgress={deploymentInProgress}
										onAction={applyAction}
										onOpenLogs={() =>
											setLogTarget({
												id: entry.id,
												title: deploymentTitle(entry.build, false),
												subtitle: deploymentSubtitle(
													entry.build,
													entry.rolloutGeneration,
													nowMs,
												),
												build: entry.build,
												allocation: status?.allocations.find(
													(allocation) =>
														allocation.desiredRolloutGeneration ===
														entry.rolloutGeneration,
												),
												rolloutGeneration: entry.rolloutGeneration,
												active: hasActiveDeployment(entry.status, entry.build),
											})
										}
									/>
								))}
							</div>
						)}
					</section>
				)}

				{deploymentsError && (
					<div
						{...stylex.props([noticeStyles.error, styles.deployRevisionError])}
					>
						{deploymentsError}
					</div>
				)}

				{!deploymentsLoading &&
					!deploymentsError &&
					liveDeployments.length === 0 &&
					previousDeployments.length === 0 && (
						<div {...stylex.props(styles.emptyState)}>
							<div {...stylex.props(styles.emptyCopy)}>
								<div {...stylex.props(styles.emptyTitle)}>
									Nothing deployed yet
								</div>
								<div {...stylex.props(styles.emptyDescription)}>
									{currentService.pendingChanges
										? "Deploy your changes to create the first deployment."
										: "Deployment activity will appear here."}
								</div>
							</div>
						</div>
					)}
			</div>

			{project && (
				<div
					aria-hidden={logTarget ? undefined : true}
					{...stylex.props([
						styles.logPanel,
						logTarget ? styles.logPanelOpen : styles.logPanelClosed,
					])}
				>
					<div {...stylex.props(styles.logHeader)}>
						<button
							type="button"
							{...stylex.props(styles.backButton)}
							onClick={() => setLogTarget(null)}
						>
							<ArrowLeft size={14} />
							Back
						</button>
						<div {...stylex.props(styles.manualDeployCopy)}>
							<div {...stylex.props(styles.logTitle)}>Deployment logs</div>
							{logTarget?.title && (
								<div {...stylex.props(styles.logSubtitle)}>
									{logTarget.title}
									{logTarget.subtitle ? ` • ${logTarget.subtitle}` : ""}
								</div>
							)}
						</div>
					</div>

					{logTarget && (
						<DeploymentLogsView
							service={currentService}
							project={project}
							build={logTarget.build}
							allocation={logTarget.allocation}
							rolloutGeneration={logTarget.rolloutGeneration}
							active={logTarget.active}
						/>
					)}
				</div>
			)}
		</div>
	);
}

const styles = stylex.create({
	panel: { position: "relative", height: "100%", overflow: "hidden" },
	summary: {
		display: "flex",
		height: "100%",
		flexDirection: "column",
		gap: "0.625rem",
		overflowY: "auto",
		padding: space.lg,
	},
	serviceMetadata: {
		display: "flex",
		minHeight: "1.75rem",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.lg,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	publicDomain: {
		display: "flex",
		minWidth: "0rem",
		alignItems: "center",
		gap: "7px",
		overflow: "hidden",
	},
	metadataIcon: { flexShrink: "0" },
	hostname: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	replicas: {
		display: "flex",
		minWidth: "0rem",
		alignItems: "center",
		gap: "7px",
	},
	manualDeployNotice: {
		display: "flex",
		minHeight: "1.75rem",
		flexWrap: "wrap",
		alignItems: "center",
		columnGap: space.sm,
		rowGap: "0.375rem",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		paddingInline: "0.625rem",
		paddingBlock: "0.375rem",
		fontSize: "0.75rem",
		lineHeight: "1.35",
	},
	manualDeployNoticeEmphasis: {
		borderColor: "rgba(80,76,71,0.55)",
		backgroundColor: "rgba(15,14,13,0.42)",
		color: colors.muted,
	},
	manualDeployCopy: { minWidth: "0rem", flex: "1" },
	pendingCommit: { fontFamily: fonts.mono, color: colors.ink },
	deployRevisionButton: { minHeight: "1.75rem", paddingInline: "11px" },
	deployRevisionError: {
		paddingInline: "9px",
		paddingBlock: "7px",
		fontSize: "11px",
	},
	liveDeployments: {
		display: "flex",
		flexDirection: "column",
		gap: "0.625rem",
	},
	history: { display: "flex", flexShrink: "0", flexDirection: "column" },
	historyToggle: {
		display: "inline-flex",
		width: "100%",
		cursor: "pointer",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		padding: "0rem",
		textAlign: "left",
		fontFamily: fonts.display,
		fontSize: "1rem",
		lineHeight: "calc(1.5 / 1)",
		fontWeight: "500",
		letterSpacing: "-0.02em",
		color: colors.ink,
	},
	historyChevron: {
		width: "0.625rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.dim,
	},
	historyCount: {
		fontFamily: fonts.mono,
		fontSize: "11px",
		fontWeight: "400",
		color: colors.dim,
	},
	historyRows: { display: "flex", flexDirection: "column" },
	emptyState: {
		backgroundImage:
			"radial-gradient(circle, rgba(126,119,110,0.32) 1px, transparent 1px)",
		backgroundSize: "14px 14px",
		display: "flex",
		minHeight: "12rem",
		flex: "1",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.xl,
		textAlign: "center",
	},
	emptyCopy: {
		backgroundColor: colors.surface,
		paddingInline: space.lg,
		paddingBlock: space.md,
	},
	emptyTitle: {
		fontFamily: fonts.display,
		fontSize: "1rem",
		lineHeight: "calc(1.5 / 1)",
		fontWeight: "500",
		color: colors.ink,
	},
	emptyDescription: {
		marginTop: space.xs,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	logPanel: {
		position: "absolute",
		inset: "0rem",
		zIndex: "2",
		display: "flex",
		flexDirection: "column",
		borderLeftStyle: "solid",
		borderLeftWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		transitionProperty: "transform, translate, scale, rotate",
		transitionTimingFunction: "cubic-bezier(0, 0, 0.2, 1)",
		transitionDuration: motion.normal,
	},
	logPanelOpen: { translate: "0rem 0" },
	logPanelClosed: { translate: "102% 0" },
	logHeader: {
		display: "flex",
		flexShrink: "0",
		alignItems: { default: "flex-start", "@media (width < 900px)": "stretch" },
		gap: space.md,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		padding: space.lg,
		flexDirection: { default: null, "@media (width < 900px)": "column" },
	},
	backButton: {
		display: "inline-flex",
		minHeight: "2rem",
		cursor: "pointer",
		alignItems: "center",
		gap: "0.375rem",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		paddingInline: "0.625rem",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.09em",
		color: colors.ink,
	},
	logTitle: {
		fontFamily: fonts.condensed,
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.08em",
		color: colors.ink,
	},
	logSubtitle: {
		marginTop: space.xs,
		fontSize: "0.75rem",
		lineHeight: "1.4",
		color: colors.muted,
	},
});
