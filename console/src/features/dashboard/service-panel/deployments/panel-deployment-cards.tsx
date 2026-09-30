import * as stylex from "@stylexjs/stylex";
import { noticeStyles } from "#/components/ui/notice";
import { DeploymentActionHistory } from "#/features/dashboard/service-panel/deployments/deployment-action-history";
import { DeploymentActionsMenu } from "#/features/dashboard/service-panel/deployments/deployment-actions-menu";
import { DeploymentCardHeader } from "#/features/dashboard/service-panel/deployments/deployment-card-header";
import { DeploymentFailure } from "#/features/dashboard/service-panel/deployments/deployment-failure";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { Loader2, XCircle } from "lucide-react";
import { useState } from "react";
import { AllocationCrashEvidence } from "#/features/dashboard/service-panel/deployments/allocation-crash-evidence";
import { DeploymentDetails } from "#/features/dashboard/service-panel/deployments/deployment-details";
import {
	buildStepHint,
	deploymentProgressCopy,
	deploymentStagesForDisplay,
	extractMissingEnvKeys,
	focusDeploymentStage,
	selectInlineLogSnippet,
	trafficRetentionCopy,
} from "#/features/dashboard/service-panel/deployments/deployment-inline";
import { useInlineDeploymentLogs } from "#/features/dashboard/service-panel/deployments/panel-deployment-logs";
import {
	actionLabel,
	availableDeploymentActions,
	deploymentMeta,
	getDeploymentCardTone,
	hasActiveDeployment,
	logLinesForStage,
	stageStatusText,
	withSourceStage,
} from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import { shortSha } from "#/features/dashboard/shared/service-utils";
import type {
	DashboardAllocationStatus,
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/errors";

export function DeploymentCard({
	service,
	record,
	logsEnabled,
	allocations,
	nowMs,
	deploymentInProgress,
	onOpenLogs,
	onOpenVariables,
	onAction,
}: {
	service: DashboardServiceRecord;
	record: DashboardDeploymentRecord;
	logsEnabled: boolean;
	allocations: Array<DashboardAllocationStatus>;
	nowMs: number;
	deploymentInProgress: boolean;
	onOpenLogs: () => void;
	onOpenVariables?: (key: string) => void;
	onAction: (
		record: DashboardDeploymentRecord,
		action: DashboardDeploymentAction,
		allocationId?: string,
	) => Promise<void>;
}) {
	const build = record.build;
	const allocation = allocations.find(
		(entry) => entry.desiredRolloutGeneration === record.rolloutGeneration,
	);
	const status = record.status;
	const reportedStages = deploymentStagesForDisplay(
		status,
		build,
		(build?.stages?.length ?? 0) > 0
			? (build?.stages ?? [])
			: (record.stages ?? []),
	);
	const stages =
		record.status?.buildReused || record.status?.reasonCode === "BUILD_REUSED"
			? reportedStages.filter(
					(stage) => !["build", "source"].includes(stage.key),
				)
			: withSourceStage(service, build, reportedStages);
	const active = hasActiveDeployment(status, build);
	const timestamp =
		status?.transitionedAt ??
		build?.startedAt ??
		build?.queuedAt ??
		build?.finishedAt ??
		allocation?.updatedAt;
	const tone = getDeploymentCardTone({
		build,
		allocation,
		active,
		isCurrent: record.isCurrent,
		status,
	});
	const meta = deploymentMeta(build, timestamp, nowMs);
	const failedStage = stages.find(
		(stage) => stage.state === "DEPLOYMENT_STAGE_STATE_FAILED",
	);
	const runningStage = stages.find(
		(stage) => stage.state === "DEPLOYMENT_STAGE_STATE_RUNNING",
	);
	const focusStage = focusDeploymentStage(stages);
	const { lines: logLines } = useInlineDeploymentLogs({
		enabled: logsEnabled && Boolean(failedStage || runningStage),
		serviceId: service.id,
		buildId: build?.buildId,
		active: Boolean(runningStage),
	});
	const snippetLines = logLinesForStage(logLines, failedStage?.key);
	const fallbackLine = failedStage
		? failedStage.detail || build?.failureReason || "Stage failed"
		: undefined;
	const snippetSource =
		snippetLines.length > 0
			? snippetLines.map((line) => line.line)
			: fallbackLine
				? [fallbackLine]
				: [];
	const snippet = selectInlineLogSnippet(snippetSource);
	const missingKeys = extractMissingEnvKeys([
		...snippet.lines,
		failedStage?.detail,
		build?.failureReason,
	]);
	const stepHint = buildStepHint(
		logLinesForStage(logLines, focusStage?.key).map((line) => line.line),
	);
	const progress = deploymentProgressCopy({
		status,
		stages,
		build,
		stepHint,
	});
	const retention = trafficRetentionCopy({
		failed: Boolean(failedStage) || tone === "failed",
		lastSuccessfulCommitSha: service.lastSuccessfulCommitSha,
	});
	const [pendingAction, setPendingAction] =
		useState<DashboardDeploymentAction>();
	const [actionError, setActionError] = useState<string>();
	const [restartAllocationId, setRestartAllocationId] = useState("");
	const actions = availableDeploymentActions(record, deploymentInProgress);
	const displayedActions = failedStage
		? actions.filter((action) => action !== "DEPLOYMENT_ACTION_RETRY")
		: actions;

	const runAction = async (
		action: DashboardDeploymentAction,
		allocationId?: string,
	) => {
		if (pendingAction) return;
		setPendingAction(action);
		setActionError(undefined);
		try {
			await onAction(record, action, allocationId);
		} catch (cause) {
			setActionError(
				formatError(cause, `Unable to ${actionLabel(action).toLowerCase()}.`),
			);
		} finally {
			setPendingAction(undefined);
		}
	};

	const showProgress =
		tone === "running" || tone === "failed" || tone === "draining";

	return (
		<section
			{...stylex.props([
				styles.card,
				logsEnabled && [styles.interactiveCard, stylex.defaultMarker()],
				logsEnabled && deploymentShellHoverStyles(tone),
				deploymentShellToneStyles(tone),
			])}
		>
			<DeploymentCardHeader
				record={record}
				tone={tone}
				metadata={meta}
				stages={stages}
				logsEnabled={logsEnabled}
				onOpenLogs={onOpenLogs}
				actions={
					displayedActions.length > 0 && (
						<DeploymentActionsMenu
							actions={displayedActions}
							pendingAction={pendingAction}
							restartAllocationId={restartAllocationId}
							allocations={allocations}
							onRestartAllocationChange={setRestartAllocationId}
							onAction={runAction}
						/>
					)
				}
			/>

			<DeploymentDetails record={record} serviceId={service.id} />
			{((record.actions?.length ?? 0) > 0 || actionError) && (
				<div {...stylex.props(styles.actionHistory)}>
					<DeploymentActionHistory record={record} />
					{actionError && (
						<div {...stylex.props([noticeStyles.error, styles.actionError])}>
							{actionError}
						</div>
					)}
				</div>
			)}

			{showProgress && (
				<div
					{...stylex.props([
						styles.progressSummary,
						tone === "running"
							? styles.runningProgress
							: tone === "failed"
								? styles.failedProgress
								: styles.neutralProgress,
					])}
				>
					<span {...stylex.props(styles.progressIcon)}>
						{tone === "failed" ? (
							<XCircle size={13} />
						) : (
							<Loader2 size={13} {...stylex.props(styles.spinner)} />
						)}
					</span>
					<span {...stylex.props(styles.progressText)}>{progress}</span>
					{focusStage && (
						<span
							{...stylex.props([
								styles.stageDuration,
								focusStage.state === "DEPLOYMENT_STAGE_STATE_FAILED"
									? styles.failedStageDuration
									: styles.neutralStageDuration,
							])}
						>
							{stageStatusText(focusStage, nowMs)}
						</span>
					)}
				</div>
			)}

			{failedStage && (
				<DeploymentFailure
					stage={failedStage}
					snippet={snippet}
					sourceLines={snippetLines}
					missingKeys={missingKeys}
					logsEnabled={logsEnabled}
					pendingAction={pendingAction}
					onOpenLogs={onOpenLogs}
					onOpenVariables={onOpenVariables}
					onRetry={() => void runAction("DEPLOYMENT_ACTION_RETRY")}
				/>
			)}

			<AllocationCrashEvidence
				serviceId={service.id}
				allocations={allocationsForDeployment(record, allocation, allocations)}
				logsEnabled={logsEnabled}
			/>

			{retention && (
				<div {...stylex.props(styles.trafficRetention)}>
					{service.lastSuccessfulCommitSha ? (
						<>
							Traffic is still on{" "}
							<span {...stylex.props(styles.lastSuccessfulCommit)}>
								{shortSha(service.lastSuccessfulCommitSha)}
							</span>{" "}
							— {retention}
						</>
					) : (
						retention
					)}
				</div>
			)}
		</section>
	);
}

function allocationsForDeployment(
	record: DashboardDeploymentRecord,
	allocation: DashboardAllocationStatus | undefined,
	allocations: Array<DashboardAllocationStatus>,
): Array<DashboardAllocationStatus> {
	const seen = new Set<string>();
	const out: Array<DashboardAllocationStatus> = [];
	const push = (entry: DashboardAllocationStatus | undefined) => {
		if (entry && !seen.has(entry.allocationId)) {
			seen.add(entry.allocationId);
			out.push(entry);
		}
	};
	push(allocation);
	// Crash evidence only covers this deployment's rollout generation: no fallback to other
	// generations, so a failed rollout never displays the previous generation's crashes.
	if (record.rolloutGeneration !== "0") {
		for (const entry of allocations) {
			if (
				entry.desiredRolloutGeneration === record.rolloutGeneration ||
				entry.appliedRolloutGeneration === record.rolloutGeneration
			) {
				push(entry);
			}
		}
	}
	return out;
}

function deploymentShellToneStyles(
	tone: string | undefined,
): stylex.StyleXStyles {
	switch (tone) {
		case "running":
			return styles.runningCard;
		case "active":
			return styles.activeCard;
		case "draining":
		case "succeeded":
			return styles.completedCard;
		case "failed":
			return styles.failedCard;
		default:
			return styles.neutralCard;
	}
}

function deploymentShellHoverStyles(
	tone: string | undefined,
): stylex.StyleXStyles {
	switch (tone) {
		case "active":
			return styles.activeCardHover;
		case "running":
			return styles.runningCardHover;
		case "draining":
		case "succeeded":
			return styles.completedCardHover;
		default:
			return styles.neutralCardHover;
	}
}

const styles = stylex.create({
	card: {
		flexShrink: "0",
		overflow: "visible",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		backgroundColor: "rgba(25,24,22,0.96)",
	},
	interactiveCard: { cursor: "pointer" },
	actionHistory: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "flex-end",
		columnGap: space.md,
		rowGap: space.sm,
		borderTopStyle: "dashed",
		borderTopWidth: "1px",
		borderStyle: "dashed",
		borderColor: "rgba(80,76,71,0.55)",
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBlock: "0.625rem",
		paddingBottom: space.md,
	},
	actionError: { paddingInline: "9px", paddingBlock: "7px", fontSize: "11px" },
	progressSummary: {
		display: "grid",
		minHeight: "34px",
		gridTemplateColumns: "16px minmax(0,1fr) auto",
		alignItems: "center",
		gap: space.sm,
		borderTopStyle: "dashed",
		borderTopWidth: "1px",
		borderStyle: "dashed",
		borderColor: "rgba(80,76,71,0.55)",
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBlock: "7px",
		fontSize: "0.75rem",
		lineHeight: "1.35",
	},
	runningProgress: {
		backgroundColor: "rgba(192,133,32,0.08)",
		color: colors.building,
	},
	failedProgress: {
		backgroundImage:
			"linear-gradient(90deg,rgba(184,66,66,0.22),transparent 18px)",
		backgroundColor: colors.failedDim,
		color: colors.failed,
	},
	neutralProgress: {
		backgroundColor: "rgba(15,14,13,0.42)",
		color: colors.muted,
	},
	progressIcon: {
		display: "inline-flex",
		alignItems: "center",
		justifyContent: "center",
	},
	spinner: { animation: `${spin} 1s linear infinite` },
	progressText: {
		minWidth: "0rem",
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	stageDuration: {
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "11px",
	},
	failedStageDuration: { color: colors.failed },
	neutralStageDuration: { color: colors.dim },
	trafficRetention: {
		borderTopStyle: "dashed",
		borderTopWidth: "1px",
		borderStyle: "dashed",
		borderColor: "rgba(80,76,71,0.7)",
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBlock: space.md,
		fontSize: "0.75rem",
		lineHeight: "1.45",
		color: colors.muted,
	},
	lastSuccessfulCommit: { fontFamily: fonts.mono, color: colors.accent },
	runningCard: {
		borderColor: "rgba(192,133,32,0.28)",
		backgroundImage:
			"linear-gradient(180deg,rgba(192,133,32,0.12),rgba(192,133,32,0.04))",
		backgroundColor: colors.surfaceRaised,
		boxShadow: "inset 3px 0 0 rgba(192,133,32,0.45)",
	},
	activeCard: {
		borderColor: "rgba(109,190,130,0.32)",
		backgroundImage:
			"linear-gradient(180deg,rgba(109,190,130,0.14),rgba(109,190,130,0.04))",
		backgroundColor: colors.surfaceRaised,
		boxShadow: "inset 3px 0 0 rgba(109,190,130,0.7)",
	},
	completedCard: {
		borderColor: "rgba(80,76,71,0.34)",
		backgroundImage:
			"linear-gradient(180deg,rgba(80,76,71,0.12),rgba(80,76,71,0.04))",
		backgroundColor: colors.surfaceRaised,
		boxShadow: "inset 3px 0 0 rgba(80,76,71,0.45)",
	},
	failedCard: {
		borderColor: "rgba(184,66,66,0.28)",
		backgroundImage:
			"linear-gradient(180deg,rgba(184,66,66,0.12),rgba(184,66,66,0.04))",
		backgroundColor: colors.surfaceRaised,
		boxShadow: "inset 3px 0 0 rgba(184,66,66,0.5)",
	},
	neutralCard: { borderColor: colors.line },
	activeCardHover: {
		borderColor: {
			default: null,
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(109,190,130,0.48)",
			},
		},
	},
	runningCardHover: {
		borderColor: {
			default: null,
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(192,133,32,0.42)",
			},
		},
	},
	completedCardHover: {
		borderColor: {
			default: null,
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(125,117,107,0.5)",
			},
		},
	},
	neutralCardHover: {
		borderColor: {
			default: null,
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(220,214,204,0.28)",
			},
		},
	},
});
