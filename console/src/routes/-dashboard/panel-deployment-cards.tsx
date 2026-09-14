import { Loader2, MoreVertical, XCircle } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { cn } from "#/lib/cn";
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
	badge,
	badgeClass,
	btnPrimary,
	btnSecondary,
	errorMsg,
	statusDotClass,
} from "#/lib/ui-classes";

import {
	buildStepHint,
	deploymentBadgeLabel,
	deploymentProgressCopy,
	deploymentStagesForDisplay,
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
	const reportedStages = deploymentStagesForDisplay(
		status,
		build,
		(build?.stages?.length ?? 0) > 0
			? (build?.stages ?? [])
			: (record.stages ?? []),
	);
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

	const healthTone =
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
			className={cn(
				"shrink-0 overflow-visible rounded-sm border bg-[rgba(25,24,22,0.96)]",
				logsEnabled && "group/shell cursor-pointer",
				logsEnabled && deploymentShellHoverClass(tone),
				deploymentShellToneClass(tone),
			)}
		>
			<div className="relative flex min-h-[52px] items-center gap-3 px-7 py-2.5 max-[900px]:flex-col max-[900px]:items-stretch max-[900px]:px-4">
				<span
					className={
						tone === "draining"
							? cn(
									"badge",
									"offline",
									badge,
									"border-[rgba(80,76,71,0.28)] bg-[rgba(80,76,71,0.18)] text-dim",
								)
							: badgeClass(healthTone)
					}
				>
					{deploymentBadgeLabel(status?.state, build)}
				</span>
				<button
					type="button"
					className="flex min-w-0 flex-1 cursor-pointer flex-col gap-1 border-0 bg-transparent p-0 text-left disabled:cursor-default"
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					<p className="m-0 overflow-hidden text-ellipsis whitespace-nowrap font-display text-[15px] font-medium leading-[1.25] tracking-[-0.02em] text-ink">
						{deploymentCardHeadline(build)}
					</p>
					<div className="flex flex-wrap items-center gap-[7px] font-mono text-[11px] text-muted">
						{meta.map((entry, index) => (
							<span key={entry} className="inline-flex items-center gap-[7px]">
								{index > 0 && (
									<span className="inline-block size-[3px] shrink-0 rounded-full bg-dim" />
								)}
								{entry}
							</span>
						))}
					</div>
				</button>
				{showStepRail && (
					<div
						className="grid h-[3px] w-[54px] shrink-0 auto-cols-fr grid-flow-col gap-0.5"
						role="img"
						aria-label="Deploy steps"
					>
						{stages.map((stage) => {
							const segmentState =
								stage.state === "DEPLOYMENT_STAGE_STATE_SUCCEEDED"
									? "building-done"
									: stage.state;
							return (
								<span
									key={stage.key || stage.label}
									className={panelBadgeSegmentClass(segmentState)}
									title={stage.label || stage.key}
								/>
							);
						})}
					</div>
				)}
				<button
					type="button"
					className={deploymentViewLogsClass(tone)}
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					View logs
				</button>
				{displayedActions.length > 0 && (
					<DeploymentActionsMenu
						className="max-[900px]:absolute max-[900px]:top-2.5 max-[900px]:right-7"
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
				<div className="flex flex-wrap items-end gap-x-3 gap-y-2 border-t border-dashed border-[rgba(80,76,71,0.55)] px-7 py-2.5 pb-3 max-[900px]:px-4">
					<DeploymentActionHistory record={record} />
					{actionError && (
						<div className={cn(errorMsg, "px-[9px] py-[7px] text-[11px]")}>
							{actionError}
						</div>
					)}
				</div>
			)}

			{showProgress && (
				<div
					className={cn(
						"grid min-h-[34px] grid-cols-[16px_minmax(0,1fr)_auto] items-center gap-2 border-t border-dashed border-[rgba(80,76,71,0.55)] px-7 py-[7px] text-xs leading-[1.35] max-[900px]:px-4",
						tone === "running"
							? "bg-[rgba(192,133,32,0.08)] text-building"
							: tone === "failed"
								? "bg-[linear-gradient(90deg,rgba(184,66,66,0.22),transparent_18px),var(--color-failed-dim)] text-failed"
								: "bg-[rgba(15,14,13,0.42)] text-muted",
					)}
				>
					<span className="inline-flex items-center justify-center">
						{tone === "failed" ? (
							<XCircle size={13} />
						) : (
							<Loader2 size={13} className="animate-spin" />
						)}
					</span>
					<span className="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap">
						{progress}
					</span>
					{focusStage && (
						<span
							className={cn(
								"whitespace-nowrap font-mono text-[11px]",
								focusStage.state === "DEPLOYMENT_STAGE_STATE_FAILED"
									? "text-failed"
									: "text-dim",
							)}
						>
							{stageStatusText(focusStage, nowMs)}
						</span>
					)}
				</div>
			)}

			{failedStage && (
				<div className="flex animate-stage-inline-in flex-col gap-2.5 px-7 pb-3.5 max-[900px]:px-4">
					{snippet.lines.length > 0 && (
						<div
							className="overflow-hidden border border-[rgba(80,76,71,0.55)] bg-[#10100f] font-mono text-[11px] leading-[1.6] shadow-[inset_0_1px_0_rgba(255,255,255,0.02)]"
							role="log"
							aria-label={`${failedStage.label || failedStage.key} logs`}
						>
							{snippet.lines.map((line, lineIndex) => {
								const source = snippetSource[lineIndex];
								return (
									<div
										key={`${source?.sequence ?? lineIndex}:${source?.observedAt?.toISOString() ?? "local"}:${line}`}
										className={cn(
											"px-2.5 py-[3px] whitespace-pre-wrap text-dim [overflow-wrap:anywhere]",
											snippet.highlightIndexes.includes(lineIndex) &&
												"bg-[rgba(184,66,66,0.16)] text-[#d27a7a] shadow-[inset_2px_0_0_var(--color-failed)]",
										)}
									>
										{line}
									</div>
								);
							})}
						</div>
					)}
					<div className="flex flex-wrap gap-2">
						{missingKeys.map((key) => (
							<button
								key={key}
								type="button"
								className={cn(btnPrimary, "min-h-7 px-[11px]")}
								onClick={() => onOpenVariables?.(key)}
							>
								Add {key}
							</button>
						))}
						<button
							type="button"
							className={cn(btnSecondary, "min-h-7 px-[11px]")}
							onClick={onOpenLogs}
							disabled={!logsEnabled}
						>
							Full log
						</button>
						<button
							type="button"
							className={cn(btnSecondary, "min-h-7 px-[11px]")}
							onClick={() => void runAction("DEPLOYMENT_ACTION_RETRY")}
							disabled={Boolean(pendingAction)}
						>
							{pendingAction === "DEPLOYMENT_ACTION_RETRY"
								? "Retrying…"
								: "Retry build"}
						</button>
					</div>
				</div>
			)}

			{retention && (
				<div className="border-t border-dashed border-[rgba(80,76,71,0.7)] px-7 py-3 text-xs leading-[1.45] text-muted max-[900px]:px-4">
					{service.lastSuccessfulCommitSha ? (
						<>
							Traffic is still on{" "}
							<span className="font-mono text-accent">
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
		<div className="border-t border-[rgba(80,76,71,0.35)]">
			<div className="flex items-center">
				<button
					type="button"
					className="grid min-h-[42px] w-full min-w-0 cursor-pointer grid-cols-[24px_minmax(0,1fr)_auto] items-center gap-2.5 border-0 bg-transparent px-7 py-2 text-left text-muted hover:bg-[rgba(255,255,255,0.025)] hover:text-ink disabled:cursor-default disabled:opacity-55 max-[900px]:px-4"
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					<span
						role="img"
						className={statusDotClass(toneToHealthClass(tone))}
						aria-label={`Status: ${toneToHealthClass(tone)}`}
					/>
					<span className="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap text-[13px]">
						{deploymentCardHeadline(build)}
					</span>
					<span className="inline-flex items-center gap-2 font-mono text-[10px] text-dim">
						{build?.commitSha && (
							<span className="font-mono">{shortSha(build.commitSha)}</span>
						)}
						{timestamp && <span>{formatRelativeAge(timestamp, nowMs)}</span>}
					</span>
				</button>
				{actions.length > 0 && (
					<DeploymentActionsMenu
						className="mr-7 max-[900px]:mr-4"
						actions={actions}
						pendingAction={pendingAction}
						onAction={runAction}
					/>
				)}
			</div>
			{actionError && (
				<div
					className={cn(
						errorMsg,
						"mr-7 mb-2 ml-[34px] px-[9px] py-[7px] text-[11px]",
					)}
				>
					{actionError}
				</div>
			)}
		</div>
	);
}

export function DeploymentActionsMenu({
	actions,
	pendingAction,
	allocations = [],
	restartAllocationId = "",
	className,
	onRestartAllocationChange,
	onAction,
}: {
	actions: Array<DashboardDeploymentAction>;
	pendingAction?: DashboardDeploymentAction;
	allocations?: Array<DashboardAllocationStatus>;
	restartAllocationId?: string;
	className?: string;
	onRestartAllocationChange?: (allocationId: string) => void;
	onAction: (
		action: DashboardDeploymentAction,
		allocationId?: string,
	) => void | Promise<void>;
}) {
	const [open, setOpen] = useState(false);
	const menuRef = useRef<HTMLDivElement>(null);
	const orderedActions = [
		...actions.filter((action) => action !== "DEPLOYMENT_ACTION_REMOVE"),
		...actions.filter((action) => action === "DEPLOYMENT_ACTION_REMOVE"),
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
		<div className={cn("relative shrink-0", className)} ref={menuRef}>
			<button
				type="button"
				className="inline-flex size-8 cursor-pointer items-center justify-center border-0 bg-transparent p-0 text-muted hover:bg-[rgba(255,255,255,0.05)] hover:text-ink focus-visible:bg-[rgba(255,255,255,0.05)] focus-visible:text-ink aria-expanded:bg-[rgba(255,255,255,0.05)] aria-expanded:text-ink"
				aria-label="Deployment actions"
				aria-expanded={open}
				onClick={() => setOpen((current) => !current)}
			>
				<MoreVertical size={20} />
			</button>
			{open && (
				<div
					className="absolute top-[calc(100%+6px)] right-0 z-30 flex min-w-[184px] flex-col rounded-sm border border-line bg-surface-raised p-1.5 shadow-[0_14px_36px_rgba(0,0,0,0.45)]"
					role="menu"
				>
					{actions.includes("DEPLOYMENT_ACTION_RESTART") &&
						allocations.length > 1 &&
						onRestartAllocationChange && (
							<label className="mb-1 flex flex-col gap-1 border-b border-[rgba(80,76,71,0.45)] px-2.5 pt-[7px] pb-2 text-[10px] tracking-[0.05em] text-dim uppercase">
								<span>Restart target</span>
								<select
									className="h-[27px] w-full border border-line bg-surface font-mono text-[10px] text-ink"
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
							className={cn(
								"min-h-9 w-full cursor-pointer border-0 bg-transparent px-2.5 text-left text-[13px] font-medium disabled:cursor-default disabled:opacity-50",
								action === "DEPLOYMENT_ACTION_REMOVE"
									? "text-failed hover:bg-[rgba(208,85,85,0.1)] hover:text-[#e08989] focus-visible:bg-[rgba(208,85,85,0.1)] focus-visible:text-[#e08989]"
									: "text-muted hover:bg-[rgba(255,255,255,0.055)] hover:text-ink focus-visible:bg-[rgba(255,255,255,0.055)] focus-visible:text-ink",
							)}
							onClick={() => {
								setOpen(false);
								void onAction(
									action,
									action === "DEPLOYMENT_ACTION_RESTART"
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
			className="flex w-full flex-wrap items-center gap-1.5 font-mono text-[10px] text-dim"
			aria-label="Deployment actions"
		>
			{record.actions.map((action) => (
				<span
					key={action.id}
					className="border border-[rgba(80,76,71,0.45)] px-[5px] py-0.5"
				>
					{actionLabel(action.action)}
					{action.allocationId ? ` ${shortId(action.allocationId)}` : ""}
				</span>
			))}
		</section>
	);
}

function deploymentShellToneClass(tone: string | undefined): string {
	switch (tone) {
		case "running":
			return "border-[rgba(192,133,32,0.28)] bg-[linear-gradient(180deg,rgba(192,133,32,0.12),rgba(192,133,32,0.04)),var(--color-surface-raised)] shadow-[inset_3px_0_0_rgba(192,133,32,0.45)]";
		case "active":
			return "border-[rgba(109,190,130,0.32)] bg-[linear-gradient(180deg,rgba(109,190,130,0.14),rgba(109,190,130,0.04)),var(--color-surface-raised)] shadow-[inset_3px_0_0_rgba(109,190,130,0.7)]";
		case "draining":
		case "succeeded":
			return "border-[rgba(80,76,71,0.34)] bg-[linear-gradient(180deg,rgba(80,76,71,0.12),rgba(80,76,71,0.04)),var(--color-surface-raised)] shadow-[inset_3px_0_0_rgba(80,76,71,0.45)]";
		case "failed":
			return "border-[rgba(184,66,66,0.28)] bg-[linear-gradient(180deg,rgba(184,66,66,0.12),rgba(184,66,66,0.04)),var(--color-surface-raised)] shadow-[inset_3px_0_0_rgba(184,66,66,0.5)]";
		default:
			return "border-line";
	}
}

function deploymentShellHoverClass(tone: string | undefined): string {
	switch (tone) {
		case "active":
			return "hover:border-[rgba(109,190,130,0.48)]";
		case "running":
			return "hover:border-[rgba(192,133,32,0.42)]";
		case "draining":
		case "succeeded":
			return "hover:border-[rgba(125,117,107,0.5)]";
		default:
			return "hover:border-[rgba(220,214,204,0.28)]";
	}
}

function deploymentViewLogsClass(tone: string | undefined): string {
	const base =
		"shrink-0 min-h-8 cursor-pointer border px-3 font-condensed text-[11px] font-bold uppercase tracking-[0.09em] disabled:cursor-default disabled:opacity-50 max-[900px]:w-full";
	switch (tone) {
		case "active":
			return cn(
				base,
				"border-[rgba(109,190,130,0.38)] bg-[rgba(109,190,130,0.1)] text-healthy hover:border-[rgba(109,190,130,0.62)] hover:bg-[rgba(109,190,130,0.2)] hover:text-[#9ad6aa] focus-visible:border-[rgba(109,190,130,0.62)] focus-visible:bg-[rgba(109,190,130,0.2)] focus-visible:text-[#9ad6aa] group-hover/shell:border-[rgba(109,190,130,0.62)] group-hover/shell:bg-[rgba(109,190,130,0.2)] group-hover/shell:text-[#9ad6aa]",
			);
		case "running":
			return cn(
				base,
				"border-[rgba(212,154,42,0.4)] bg-[rgba(212,154,42,0.1)] text-building hover:border-[rgba(212,154,42,0.64)] hover:bg-[rgba(212,154,42,0.2)] hover:text-[#e4b65a] focus-visible:border-[rgba(212,154,42,0.64)] focus-visible:bg-[rgba(212,154,42,0.2)] focus-visible:text-[#e4b65a] group-hover/shell:border-[rgba(212,154,42,0.64)] group-hover/shell:bg-[rgba(212,154,42,0.2)] group-hover/shell:text-[#e4b65a]",
			);
		case "draining":
		case "succeeded":
			return cn(
				base,
				"border-[rgba(125,117,107,0.42)] bg-[rgba(80,76,71,0.22)] text-muted hover:border-[rgba(183,174,162,0.45)] hover:bg-[rgba(108,102,94,0.32)] hover:text-ink focus-visible:border-[rgba(183,174,162,0.45)] focus-visible:bg-[rgba(108,102,94,0.32)] focus-visible:text-ink group-hover/shell:border-[rgba(183,174,162,0.45)] group-hover/shell:bg-[rgba(108,102,94,0.32)] group-hover/shell:text-ink",
			);
		case "failed":
			return cn(
				base,
				"border-[rgba(208,85,85,0.38)] bg-[rgba(208,85,85,0.1)] text-failed hover:border-[rgba(208,85,85,0.6)] hover:bg-[rgba(208,85,85,0.2)] hover:text-[#e08989] focus-visible:border-[rgba(208,85,85,0.6)] focus-visible:bg-[rgba(208,85,85,0.2)] focus-visible:text-[#e08989] group-hover/shell:border-[rgba(208,85,85,0.6)] group-hover/shell:bg-[rgba(208,85,85,0.2)] group-hover/shell:text-[#e08989]",
			);
		default:
			return cn(
				base,
				"border-line bg-[rgba(255,255,255,0.03)] text-ink hover:bg-surface-hover focus-visible:bg-surface-hover",
			);
	}
}

function panelBadgeSegmentClass(state: string): string {
	const base =
		"rounded-full transition-[background-color] duration-[280ms] ease-in-out";
	switch (state) {
		case "DEPLOYMENT_STAGE_STATE_SUCCEEDED":
			return cn(base, "bg-healthy");
		case "building-done":
			return cn(base, "bg-building");
		case "DEPLOYMENT_STAGE_STATE_RUNNING":
			return cn(base, "bg-building animate-pulse-building");
		case "DEPLOYMENT_STAGE_STATE_FAILED":
			return cn(base, "bg-failed");
		default:
			return cn(base, "bg-[rgba(80,76,71,0.7)]");
	}
}
