import { Loader2, MoreVertical, XCircle } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDeploymentStatus,
	DashboardServiceLogLine,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import {
	buildStepHint,
	deploymentBadgeLabel,
	deploymentProgressCopy,
	extractMissingEnvKeys,
	focusDeploymentStage,
	selectInlineLogSnippet,
	trafficRetentionCopy,
} from "./deployment-inline";
import { useInlineDeploymentLogs } from "./panel-deployment-logs";
import {
	actionLabel,
	availableDeploymentActions,
	deploymentCardHeadline,
	deploymentMeta,
	formatError,
	formatRelativeAge,
	getDeploymentCardTone,
	hasActiveDeployment,
	logLinesForStage,
	stageStatusText,
	toneToHealthClass,
	withSourceStage,
} from "./panel-deployments-helpers";
import { shortId, shortSha } from "./service-utils";

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
	const allocation = record.allocation;
	const status = record.status;
	const reportedStages = build?.stages ?? record.stages ?? [];
	const stages = withSourceStage(service, build, reportedStages);
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
	const meta = deploymentMeta(build, status, timestamp, nowMs);
	const failedStage = stages.find((stage) => stage.state === "failed");
	const runningStage = stages.find((stage) => stage.state === "running");
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
			? snippetLines
			: fallbackLine
				? [
						{
							observedAt: undefined,
							allocationId: "",
							agentId: "",
							stream: "stderr",
							rolloutGeneration: 0,
							sequence: 0,
							line: fallbackLine,
						} satisfies DashboardServiceLogLine,
					]
				: [];
	const snippet = selectInlineLogSnippet(
		snippetSource.map((line) => line.line),
	);
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
		? actions.filter((action) => action !== "retry")
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

	const badgeClass =
		tone === "failed"
			? "failed"
			: tone === "running"
				? "building"
				: tone === "draining"
					? "offline"
					: "healthy";
	const showStepRail = tone === "running" && stages.length > 0;
	const showProgress =
		tone === "running" || tone === "failed" || tone === "draining";

	return (
		<section
			className={`deployment-shell deployment-shell-live ${tone ? `tone-${tone}` : ""}${
				logsEnabled ? " clickable" : ""
			}`}
		>
			<div className="deployment-live-head">
				<span className={`badge ${badgeClass}`}>
					{deploymentBadgeLabel(status?.state, build)}
				</span>
				<button
					type="button"
					className="deployment-live-copy"
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					<p className="deployment-live-message">
						{deploymentCardHeadline(build)}
					</p>
					<div className="deployment-live-meta">
						{meta.map((entry, index) => (
							<span key={entry}>
								{index > 0 && <span className="deployment-inline-dot" />}
								{entry}
							</span>
						))}
					</div>
				</button>
				{showStepRail && (
					<div
						className="panel-badge-rail"
						role="img"
						aria-label="Deploy steps"
					>
						{stages.map((stage) => {
							const segmentState =
								stage.state === "succeeded" ? "building-done" : stage.state;
							return (
								<span
									key={stage.key || stage.label}
									className={`panel-badge-segment ${segmentState}`}
									title={stage.label || stage.key}
								/>
							);
						})}
					</div>
				)}
				<button
					type="button"
					className="deployment-view-logs"
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					View logs
				</button>
				{displayedActions.length > 0 && (
					<DeploymentActionsMenu
						actions={displayedActions}
						pendingAction={pendingAction}
						restartAllocationId={restartAllocationId}
						allocations={allocations}
						onRestartAllocationChange={setRestartAllocationId}
						onAction={runAction}
					/>
				)}
			</div>

			{((record.actions?.length ?? 0) > 0 || actionError) && (
				<div className="deployment-actions">
					<DeploymentActionHistory record={record} />
					{actionError && (
						<div className="deployment-error compact">{actionError}</div>
					)}
				</div>
			)}

			{showProgress && (
				<div className={`deployment-inline-progress ${tone ?? ""}`}>
					<span className="deployment-inline-progress-icon">
						{tone === "failed" ? (
							<XCircle size={13} />
						) : (
							<Loader2
								size={13}
								style={{ animation: "spin 1s linear infinite" }}
							/>
						)}
					</span>
					<span className="deployment-inline-progress-text">{progress}</span>
					{focusStage && (
						<span
							className={`deployment-inline-progress-time${
								focusStage.state === "failed" ? " failed" : ""
							}`}
						>
							{stageStatusText(focusStage, nowMs)}
						</span>
					)}
				</div>
			)}

			{failedStage && (
				<div className="stage-inline">
					{snippet.lines.length > 0 && (
						<div
							className="stage-inline-log"
							role="log"
							aria-label={`${failedStage.label || failedStage.key} logs`}
						>
							{snippet.lines.map((line, lineIndex) => {
								const source = snippetSource[lineIndex];
								return (
									<div
										key={`${source?.sequence ?? lineIndex}:${source?.observedAt?.toISOString() ?? "local"}:${line}`}
										className={`stage-inline-line${
											snippet.highlightIndexes.includes(lineIndex)
												? " error"
												: ""
										}`}
									>
										{line}
									</div>
								);
							})}
						</div>
					)}
					<div className="stage-inline-actions">
						{missingKeys.map((key) => (
							<button
								key={key}
								type="button"
								className="btn-primary"
								onClick={() => onOpenVariables?.(key)}
							>
								Add {key}
							</button>
						))}
						<button
							type="button"
							className="btn-secondary"
							onClick={onOpenLogs}
							disabled={!logsEnabled}
						>
							Full log
						</button>
						<button
							type="button"
							className="btn-secondary"
							onClick={() => void runAction("retry")}
							disabled={Boolean(pendingAction)}
						>
							{pendingAction === "retry" ? "Retrying…" : "Retry build"}
						</button>
					</div>
				</div>
			)}

			{retention && (
				<div className="deployment-retention">
					{service.lastSuccessfulCommitSha ? (
						<>
							Traffic is still on{" "}
							<span className="mono deployment-retention-sha">
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

export function DeploymentHistoryRow({
	record,
	build,
	allocation,
	status,
	logsEnabled,
	nowMs,
	deploymentInProgress,
	onOpenLogs,
	onAction,
}: {
	record: DashboardDeploymentRecord;
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	status?: DashboardDeploymentStatus;
	logsEnabled: boolean;
	nowMs: number;
	deploymentInProgress: boolean;
	onOpenLogs: () => void;
	onAction: (
		record: DashboardDeploymentRecord,
		action: DashboardDeploymentAction,
	) => Promise<void>;
}) {
	const tone =
		getDeploymentCardTone({
			build,
			allocation,
			active: hasActiveDeployment(status, build),
			isCurrent: false,
			status,
		}) ?? "draining";
	const timestamp =
		build?.startedAt ??
		build?.queuedAt ??
		build?.finishedAt ??
		allocation?.updatedAt;
	const [pendingAction, setPendingAction] =
		useState<DashboardDeploymentAction>();
	const [actionError, setActionError] = useState<string>();
	const actions = availableDeploymentActions(record, deploymentInProgress);

	const runAction = async (action: DashboardDeploymentAction) => {
		if (pendingAction) return;
		setPendingAction(action);
		setActionError(undefined);
		try {
			await onAction(record, action);
		} catch (cause) {
			setActionError(
				formatError(cause, `Unable to ${actionLabel(action).toLowerCase()}.`),
			);
		} finally {
			setPendingAction(undefined);
		}
	};

	return (
		<div className="deployment-history-entry">
			<div className="deployment-history-row-shell">
				<button
					type="button"
					className="deployment-history-row"
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					<span className={`status-dot ${toneToHealthClass(tone)}`} />
					<span className="deployment-history-message">
						{deploymentCardHeadline(build)}
					</span>
					<span className="deployment-history-meta">
						{build?.commitSha && (
							<span className="mono">{shortSha(build.commitSha)}</span>
						)}
						{timestamp && <span>{formatRelativeAge(timestamp, nowMs)}</span>}
					</span>
				</button>
				{actions.length > 0 && (
					<DeploymentActionsMenu
						actions={actions}
						pendingAction={pendingAction}
						onAction={runAction}
					/>
				)}
			</div>
			<DeploymentActionHistory record={record} />
			{actionError && (
				<div className="deployment-error compact">{actionError}</div>
			)}
		</div>
	);
}

export function DeploymentActionsMenu({
	actions,
	pendingAction,
	allocations = [],
	restartAllocationId = "",
	onRestartAllocationChange,
	onAction,
}: {
	actions: Array<DashboardDeploymentAction>;
	pendingAction?: DashboardDeploymentAction;
	allocations?: Array<DashboardAllocationStatus>;
	restartAllocationId?: string;
	onRestartAllocationChange?: (allocationId: string) => void;
	onAction: (
		action: DashboardDeploymentAction,
		allocationId?: string,
	) => void | Promise<void>;
}) {
	const [open, setOpen] = useState(false);
	const menuRef = useRef<HTMLDivElement>(null);
	const orderedActions = [
		...actions.filter((action) => action !== "remove"),
		...actions.filter((action) => action === "remove"),
	];

	useEffect(() => {
		if (!open) return;
		const onPointerDown = (event: MouseEvent) => {
			if (!menuRef.current?.contains(event.target as Node)) setOpen(false);
		};
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key === "Escape") setOpen(false);
		};
		document.addEventListener("mousedown", onPointerDown);
		document.addEventListener("keydown", onKeyDown);
		return () => {
			document.removeEventListener("mousedown", onPointerDown);
			document.removeEventListener("keydown", onKeyDown);
		};
	}, [open]);

	return (
		<div className="deployment-menu" ref={menuRef}>
			<button
				type="button"
				className="deployment-menu-trigger"
				aria-label="Deployment actions"
				aria-expanded={open}
				onClick={() => setOpen((current) => !current)}
			>
				<MoreVertical size={20} />
			</button>
			{open && (
				<div className="deployment-menu-popover" role="menu">
					{actions.includes("restart") &&
						allocations.length > 1 &&
						onRestartAllocationChange && (
							<label className="deployment-restart-target">
								<span>Restart target</span>
								<select
									value={restartAllocationId}
									onChange={(event) =>
										onRestartAllocationChange(event.target.value)
									}
									disabled={Boolean(pendingAction)}
								>
									<option value="">All replicas</option>
									{allocations.map((entry) => (
										<option key={entry.allocationId} value={entry.allocationId}>
											{shortId(entry.allocationId)} on {shortId(entry.agentId)}
										</option>
									))}
								</select>
							</label>
						)}
					{orderedActions.map((action) => (
						<button
							key={action}
							type="button"
							role="menuitem"
							className={action === "remove" ? "danger" : undefined}
							onClick={() => {
								setOpen(false);
								void onAction(
									action,
									action === "restart"
										? restartAllocationId || undefined
										: undefined,
								);
							}}
							disabled={Boolean(pendingAction)}
						>
							{pendingAction === action ? "Working…" : actionLabel(action)}
						</button>
					))}
				</div>
			)}
		</div>
	);
}

export function DeploymentActionHistory({
	record,
}: {
	record: DashboardDeploymentRecord;
}) {
	if (!record.actions?.length) return null;
	return (
		<section
			className="deployment-action-history"
			aria-label="Deployment actions"
		>
			{record.actions.map((action) => (
				<span key={action.id}>
					{actionLabel(action.action)}
					{action.allocationId ? ` ${shortId(action.allocationId)}` : ""}
				</span>
			))}
		</section>
	);
}
