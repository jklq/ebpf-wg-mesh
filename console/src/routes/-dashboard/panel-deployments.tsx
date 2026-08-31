import {
	ArrowLeft,
	Boxes,
	EyeOff,
	Globe2,
	Loader2,
	RefreshCw,
	Search,
	XCircle,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardDeploymentRecord,
	DashboardDeploymentStage,
	DashboardDeploymentStatus,
	DashboardDomainBinding,
	DashboardProject,
	DashboardServiceLogLine,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import {
	buildStepHint,
	deploymentBadgeLabel,
	deploymentCauseLabel,
	deploymentProgressCopy,
	extractMissingEnvKeys,
	focusDeploymentStage,
	isInProgressDeploymentState,
	partitionDeployments,
	selectInlineLogSnippet,
	trafficRetentionCopy,
} from "./deployment-inline";
import {
	doRedeployService,
	fetchServiceDeployments,
	fetchServiceLogs,
} from "./server-fns";
import { shortId, shortSha } from "./service-utils";
import { usePolling } from "./use-polling";

type LogTypeFilter = "all" | "deploy" | "build" | "runtime";
type DeploymentCardTone = "running" | "active" | "draining" | "failed";

type DeploymentLogTarget = {
	id: string;
	title: string;
	subtitle?: string;
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	rolloutGeneration?: number;
	active: boolean;
};

export function PanelDeployments({
	service,
	status,
	project,
	domains = [],
	onOpenVariables,
	onRedeployed,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
	domains?: DashboardDomainBinding[];
	onOpenVariables?: (key: string) => void;
	onRedeployed?: (status: DashboardServiceStatus) => void;
}) {
	const currentService = status?.service ?? service;
	const deploymentStatus =
		currentService.latestDeployment ?? service.latestDeployment;
	const build = currentService.latestBuild ?? service.latestBuild;
	const allocation = status?.allocation;
	const publicDomain = domains.find(
		(binding) =>
			binding.serviceId === currentService.id &&
			binding.ownershipState !== "unverified",
	)?.hostname;
	const replicaCount = currentService.desiredReplicaCount ?? 1;
	const rolloutGeneration =
		deploymentStatus?.rolloutGeneration ||
		allocation?.desiredRolloutGeneration ||
		currentService.rolloutGeneration;
	const [deployments, setDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [sessionDeployments, setSessionDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [deploymentsError, setDeploymentsError] = useState<string>();
	const [logTarget, setLogTarget] = useState<DeploymentLogTarget | null>(null);
	const [historyOpen, setHistoryOpen] = useState(false);
	const [nowMs, setNowMs] = useState(() => Date.now());
	const lastObservedDeployment = useRef<DashboardDeploymentRecord | null>(null);

	const loadDeployments = useCallback(async () => {
		if (!project) {
			setDeployments([]);
			setDeploymentsError(undefined);
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
					? nextDeployments
							.map(hydrateDeploymentRecord)
							.sort(compareDeploymentsNewestFirst)
					: [],
			);
			setDeploymentsError(undefined);
		} catch (cause) {
			setDeployments([]);
			setDeploymentsError(formatError(cause, "Unable to load deployments."));
		}
	}, [project, service.id]);

	useEffect(() => {
		void loadDeployments();
	}, [loadDeployments]);

	useEffect(() => {
		const id = window.setInterval(() => setNowMs(Date.now()), 30_000);
		return () => window.clearInterval(id);
	}, []);

	const currentRecord = useMemo(
		() =>
			createDeploymentRecord({
				serviceId: service.id,
				build,
				allocation,
				rolloutGeneration,
				isCurrent: true,
				status: deploymentStatus,
			}),
		[service.id, build, allocation, rolloutGeneration, deploymentStatus],
	);

	const mergedDeployments = useMemo(
		() =>
			mergeDeploymentRecords([
				...sessionDeployments,
				...deployments,
				currentRecord,
			]),
		[sessionDeployments, deployments, currentRecord],
	);

	const { live: liveDeployments, history: previousDeployments } =
		useMemo(() => {
			const partitioned = partitionDeployments(mergedDeployments);
			return {
				live: partitioned.live.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
				history: partitioned.history.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
			};
		}, [mergedDeployments, currentService]);

	const shouldPollDeployments = liveDeployments.some(
		(entry) =>
			isInProgressDeploymentState(entry.status?.state) ||
			hasActiveDeployment(entry.status, entry.build),
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
		const previousDeployment = lastObservedDeployment.current;

		if (
			previousDeployment &&
			!isSameDeploymentRecord(previousDeployment, currentRecord) &&
			hasDeploymentIdentity(previousDeployment)
		) {
			setSessionDeployments((currentSessionDeployments) =>
				mergeDeploymentRecords([
					...currentSessionDeployments,
					{
						...previousDeployment,
						isCurrent: false,
					},
				]),
			);
		}

		lastObservedDeployment.current = currentRecord;
	}, [currentRecord]);

	return (
		<div className="deployments-panel">
			<div className="deployments-list">
				<div className="deployments-summary">
					<div className="deployments-summary-item">
						{publicDomain ? <Globe2 size={15} /> : <EyeOff size={15} />}
						<span>{publicDomain ?? "Unexposed service"}</span>
					</div>
					<div className="deployments-summary-item">
						<Boxes size={15} />
						<span>
							{replicaCount} {replicaCount === 1 ? "Replica" : "Replicas"}
						</span>
					</div>
				</div>
				<div className="deployment-live-list">
					{liveDeployments.map((entry) => (
						<DeploymentCard
							key={deploymentRecordKey(entry)}
							service={currentService}
							record={entry}
							logsEnabled={Boolean(project)}
							nowMs={nowMs}
							onOpenVariables={onOpenVariables}
							onRedeployed={onRedeployed}
							onOpenLogs={() =>
								setLogTarget({
									id: deploymentRecordKey(entry),
									title: deploymentTitle(entry.build, entry.isCurrent),
									subtitle: deploymentSubtitle(
										entry.build,
										entry.rolloutGeneration,
										nowMs,
									),
									build: entry.build,
									allocation: entry.allocation,
									rolloutGeneration: entry.rolloutGeneration,
									active: hasActiveDeployment(entry.status, entry.build),
								})
							}
						/>
					))}
				</div>

				{previousDeployments.length > 0 && (
					<section className="deployment-history">
						<button
							type="button"
							className="deployment-history-toggle"
							aria-expanded={historyOpen}
							onClick={() => setHistoryOpen((open) => !open)}
						>
							<span className="deployment-history-chevron" aria-hidden>
								{historyOpen ? "▾" : "▸"}
							</span>
							History
							<span className="deployment-history-count">
								{previousDeployments.length}
							</span>
						</button>
						{historyOpen && (
							<div className="deployment-history-list">
								{previousDeployments.map((entry) => (
									<DeploymentHistoryRow
										key={deploymentRecordKey(entry)}
										build={entry.build}
										allocation={entry.allocation}
										status={entry.status}
										logsEnabled={Boolean(project)}
										nowMs={nowMs}
										onOpenLogs={() =>
											setLogTarget({
												id: deploymentRecordKey(entry),
												title: deploymentTitle(entry.build, false),
												subtitle: deploymentSubtitle(
													entry.build,
													entry.rolloutGeneration,
													nowMs,
												),
												build: entry.build,
												allocation: entry.allocation,
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
					<div className="deployment-error compact">{deploymentsError}</div>
				)}
			</div>

			<div className={`deployment-log-drawer ${logTarget ? "open" : ""}`}>
				<div className="deployment-log-drawer-header">
					<button
						type="button"
						className="deployment-log-back"
						onClick={() => setLogTarget(null)}
					>
						<ArrowLeft size={14} />
						Back
					</button>
					<div className="deployment-log-drawer-copy">
						<div className="deployment-log-drawer-title">Deployment logs</div>
						{logTarget?.title && (
							<div className="deployment-log-drawer-subtitle">
								{logTarget.title}
								{logTarget.subtitle ? ` • ${logTarget.subtitle}` : ""}
							</div>
						)}
					</div>
				</div>

				{logTarget && project && (
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
		</div>
	);
}

function DeploymentCard({
	service,
	record,
	logsEnabled,
	nowMs,
	onOpenLogs,
	onOpenVariables,
	onRedeployed,
}: {
	service: DashboardServiceRecord;
	record: DashboardDeploymentRecord;
	logsEnabled: boolean;
	nowMs: number;
	onOpenLogs: () => void;
	onOpenVariables?: (key: string) => void;
	onRedeployed?: (status: DashboardServiceStatus) => void;
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
	const [retrying, setRetrying] = useState(false);
	const [retryError, setRetryError] = useState<string>();

	const retryBuild = async () => {
		if (retrying) return;
		setRetrying(true);
		setRetryError(undefined);
		try {
			const next = await doRedeployService({
				data: { serviceId: service.id },
			});
			onRedeployed?.(next);
		} catch (cause) {
			setRetryError(formatError(cause, "Unable to retry the build."));
		} finally {
			setRetrying(false);
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
			</div>

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
							onClick={() => void retryBuild()}
							disabled={retrying}
						>
							{retrying ? "Retrying…" : "Retry build"}
						</button>
					</div>
					{retryError && (
						<div className="deployment-error compact">{retryError}</div>
					)}
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

function useInlineDeploymentLogs({
	enabled,
	serviceId,
	buildId,
	active,
}: {
	enabled: boolean;
	serviceId: string;
	buildId?: string;
	active: boolean;
}) {
	const [lines, setLines] = useState<Array<DashboardServiceLogLine>>([]);

	const loadLogs = useCallback(async () => {
		if (!enabled) {
			setLines([]);
			return;
		}
		try {
			const nextLines = await fetchServiceLogs({
				data: {
					serviceId,
					limit: 80,
					buildId,
				},
			});
			if (!Array.isArray(nextLines)) {
				setLines([]);
				return;
			}
			setLines(
				nextLines.map(hydrateServiceLogLine).sort((left, right) => {
					const leftTime = left.observedAt?.getTime() ?? 0;
					const rightTime = right.observedAt?.getTime() ?? 0;
					return leftTime - rightTime || left.sequence - right.sequence;
				}),
			);
		} catch {
			setLines([]);
		}
	}, [enabled, serviceId, buildId]);

	useEffect(() => {
		void loadLogs();
	}, [loadLogs]);

	usePolling(loadLogs, { enabled: enabled && active, intervalMs: 2000 });

	return { lines };
}

function logLinesForStage(
	lines: Array<DashboardServiceLogLine>,
	stageKey: string | undefined,
): Array<DashboardServiceLogLine> {
	const matching = stageKey
		? lines.filter((line) => !line.stage || line.stage === stageKey)
		: lines;
	const source = matching.length > 0 ? matching : lines;
	return source.filter((line) => line.line.trim() !== "");
}

function withSourceStage(
	service: DashboardServiceRecord,
	build: DashboardBuildStatus | undefined,
	stages: Array<DashboardDeploymentStage>,
): Array<DashboardDeploymentStage> {
	if (
		stages.some(
			(stage) => stage.key === "initialization" || stage.key === "source",
		)
	) {
		return stages;
	}
	const source = service.spec?.source;
	if (!source?.repositorySelector) {
		return stages;
	}
	const sha = build?.commitSha ? shortSha(build.commitSha) : undefined;
	const repo =
		source.repositorySelector.split("/").slice(-2).join("/") ||
		source.repositorySelector;
	return [
		{
			key: "source",
			label: "Source",
			detail: sha ? `${repo} @ ${sha}` : repo,
			state: "succeeded",
			startedAt: build?.queuedAt ?? build?.startedAt,
			finishedAt: build?.startedAt ?? build?.queuedAt,
		},
		...stages,
	];
}

function DeploymentHistoryRow({
	build,
	allocation,
	status,
	logsEnabled,
	nowMs,
	onOpenLogs,
}: {
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	status?: DashboardDeploymentStatus;
	logsEnabled: boolean;
	nowMs: number;
	onOpenLogs: () => void;
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

	return (
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
	);
}

function DeploymentLogsView({
	service,
	project,
	build,
	allocation,
	rolloutGeneration,
	active,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject | undefined;
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	rolloutGeneration?: number;
	active: boolean;
}) {
	const [search, setSearch] = useState("");
	const [debouncedSearch, setDebouncedSearch] = useState("");
	const [filter, setFilter] = useState<LogTypeFilter>("all");
	const [lines, setLines] = useState<Array<DashboardServiceLogLine>>([]);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<string>();

	useEffect(() => {
		const id = window.setTimeout(() => setDebouncedSearch(search.trim()), 250);
		return () => window.clearTimeout(id);
	}, [search]);

	const loadLogs = useCallback(async () => {
		if (!project) return;
		setLoading(true);
		setError(undefined);
		try {
			const logType = filter === "all" ? undefined : filter;
			const nextLines = await fetchServiceLogs({
				data: {
					serviceId: service.id,
					limit: 500,
					logType,
					allocationId:
						filter === "runtime" ? allocation?.allocationId : undefined,
					buildId:
						build?.buildId && (filter === "deploy" || filter === "build")
							? build.buildId
							: undefined,
					search: debouncedSearch || undefined,
				},
			});
			if (!Array.isArray(nextLines)) {
				throw new Error("Log response did not include a line list.");
			}
			setLines(nextLines.map(hydrateServiceLogLine));
		} catch (cause) {
			setError(formatError(cause, "Unable to load logs."));
			setLines([]);
		} finally {
			setLoading(false);
		}
	}, [
		project,
		service.id,
		filter,
		allocation?.allocationId,
		build?.buildId,
		debouncedSearch,
	]);

	useEffect(() => {
		void loadLogs();
	}, [loadLogs]);

	usePolling(loadLogs, { enabled: active, intervalMs: 2000 });

	const filteredLines = useMemo(
		() =>
			lines.filter((line) =>
				matchesDeploymentLog(line, {
					buildId: build?.buildId,
					allocationId: allocation?.allocationId,
					rolloutGeneration,
				}),
			),
		[lines, build?.buildId, allocation?.allocationId, rolloutGeneration],
	);

	const sortedLines = useMemo(
		() =>
			[...filteredLines].sort((a, b) => {
				const aTime = a.observedAt?.getTime() ?? 0;
				const bTime = b.observedAt?.getTime() ?? 0;
				return aTime - bTime || a.sequence - b.sequence;
			}),
		[filteredLines],
	);

	const emptyText =
		search || filter !== "all"
			? "No logs match this filter."
			: "Logs will appear here as this deployment progresses.";

	return (
		<div className="logs-view">
			<div className="logs-toolbar">
				<label className="logs-search">
					<Search size={13} />
					<input
						value={search}
						onChange={(event) => setSearch(event.target.value)}
						placeholder="Search logs"
					/>
				</label>
				<button
					type="button"
					className="logs-refresh"
					onClick={() => void loadLogs()}
					disabled={loading}
					aria-label="Refresh logs"
					title="Refresh logs"
				>
					<RefreshCw size={13} className={loading ? "spinning" : ""} />
				</button>
			</div>

			<div className="log-filters">
				{(["all", "deploy", "build", "runtime"] as const).map((type) => (
					<button
						key={type}
						type="button"
						className={filter === type ? "active" : ""}
						onClick={() => setFilter(type)}
					>
						{type}
					</button>
				))}
			</div>

			{error && <div className="deployment-error compact">{error}</div>}

			<div className="terminal-log">
				{sortedLines.length === 0 && !loading && (
					<div className="deployment-empty logs">{emptyText}</div>
				)}
				{sortedLines.map((line) => (
					<div
						key={`${line.observedAt?.toISOString() ?? "t"}-${line.sequence}-${line.line}`}
						className="log-line"
					>
						<div className="log-meta">
							<span className="log-time">
								{line.observedAt ? formatLogTime(line.observedAt) : "--:--:--"}
							</span>
							<span className={`log-badge ${line.logType ?? "unspecified"}`}>
								{line.logType ?? "log"}
							</span>
						</div>
						<span className="log-body">{line.line}</span>
					</div>
				))}
				{loading && sortedLines.length === 0 && (
					<div className="deployment-empty logs">Loading logs...</div>
				)}
			</div>
		</div>
	);
}

function getDeploymentCardTone({
	build,
	allocation,
	active,
	isCurrent,
	status,
}: {
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	active: boolean;
	isCurrent: boolean;
	status?: DashboardDeploymentStatus;
}): DeploymentCardTone | undefined {
	if (
		status?.state === "failed" ||
		status?.state === "crashed" ||
		status?.state === "cancelled"
	) {
		return "failed";
	}
	if (status?.state === "active") {
		return "active";
	}
	if (status?.state === "draining" || status?.state === "completed") {
		return "draining";
	}
	if (
		status &&
		status.state !== "superseded" &&
		status.state !== "removed" &&
		status.state !== "unspecified"
	) {
		return "running";
	}

	const failedStage = build?.stages?.find((stage) => stage.state === "failed");
	const failed =
		Boolean(failedStage) ||
		build?.state === "failed" ||
		Boolean(allocation?.message && !allocation.healthy && !active);

	if (failed) {
		return "failed";
	}

	if (active) {
		return "running";
	}

	if (isCurrent && !active && !failed && allocation?.healthy) {
		return "active";
	}

	if (!isCurrent && build?.state === "succeeded") {
		return "draining";
	}

	return undefined;
}

function stageStatusText(
	stage: DashboardDeploymentStage,
	nowMs?: number,
): string {
	if (stage.startedAt && stage.finishedAt) {
		return formatDuration(
			stage.finishedAt.getTime() - stage.startedAt.getTime(),
		);
	}
	if (stage.state === "running" && stage.startedAt && nowMs) {
		return formatDuration(nowMs - stage.startedAt.getTime());
	}
	switch (stage.state) {
		case "running":
			return "In progress";
		case "pending":
			return "Not started";
		case "skipped":
			return "Skipped";
		case "failed":
			return "Failed";
		case "succeeded":
			return "Done";
		default:
			return "Waiting";
	}
}

function hasActiveDeployment(
	status: DashboardDeploymentStatus | undefined,
	build: DashboardBuildStatus | undefined,
): boolean {
	if (status) {
		switch (status.state) {
			case "completed":
			case "failed":
			case "cancelled":
			case "crashed":
			case "removed":
			case "superseded":
			case "active":
				return false;
			default:
				return true;
		}
	}
	return Boolean(build?.state === "queued" || build?.state === "running");
}

function deploymentRecordKey(entry: DashboardDeploymentRecord): string {
	return entry.build?.buildId || `${entry.id}-${entry.rolloutGeneration}`;
}

function compareDeploymentsNewestFirst(
	a: DashboardDeploymentRecord,
	b: DashboardDeploymentRecord,
): number {
	return deploymentTime(b) - deploymentTime(a);
}

function mergeDeploymentRecords(
	records: Array<DashboardDeploymentRecord>,
): Array<DashboardDeploymentRecord> {
	const uniqueRecords = new Map<string, DashboardDeploymentRecord>();
	for (const record of records) {
		uniqueRecords.set(deploymentRecordKey(record), record);
	}
	return [...uniqueRecords.values()].sort(compareDeploymentsNewestFirst);
}

function deploymentTime(entry: DashboardDeploymentRecord): number {
	return (
		entry.createdAt?.getTime() ??
		entry.build?.startedAt?.getTime() ??
		entry.build?.queuedAt?.getTime() ??
		entry.build?.finishedAt?.getTime() ??
		entry.allocation?.updatedAt?.getTime() ??
		0
	);
}

function isSameDeploymentRecord(
	left: DashboardDeploymentRecord,
	right: DashboardDeploymentRecord,
): boolean {
	if (left.build?.buildId && right.build?.buildId) {
		return left.build.buildId === right.build.buildId;
	}
	return (
		left.rolloutGeneration !== undefined &&
		right.rolloutGeneration !== undefined &&
		left.rolloutGeneration === right.rolloutGeneration
	);
}

function hasDeploymentIdentity(entry: DashboardDeploymentRecord): boolean {
	return Boolean(
		entry.build?.buildId !== undefined || entry.rolloutGeneration !== undefined,
	);
}

function createDeploymentRecord({
	serviceId,
	build,
	allocation,
	rolloutGeneration,
	isCurrent,
	status,
}: {
	serviceId: string;
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	rolloutGeneration?: number;
	isCurrent: boolean;
	status?: DashboardDeploymentStatus;
}): DashboardDeploymentRecord {
	return {
		id: status?.deploymentId || serviceId,
		rolloutGeneration: status?.rolloutGeneration || rolloutGeneration || 0,
		createdAt:
			status?.transitionedAt ??
			build?.startedAt ??
			build?.queuedAt ??
			build?.finishedAt ??
			allocation?.updatedAt,
		build,
		allocation,
		isCurrent,
		status,
		imageDigest: status?.imageDigest,
	};
}

function shouldRenderDeploymentHistoryEntry(
	entry: DashboardDeploymentRecord,
	service: DashboardServiceRecord,
): boolean {
	if (!usesRepositorySource(service)) {
		return true;
	}
	return Boolean(entry.build?.buildId);
}

function usesRepositorySource(service: DashboardServiceRecord): boolean {
	return Boolean(
		service.spec?.source?.provider || service.sourceSummary?.desiredSpec,
	);
}

function deploymentTitle(
	build: DashboardBuildStatus | undefined,
	isCurrent: boolean,
): string {
	if (build?.commitMessage?.trim()) return build.commitMessage.trim();
	if (build?.commitSha) return `Commit ${shortSha(build.commitSha)}`;
	if (build?.buildId) return `Build ${shortId(build.buildId)}`;
	return isCurrent ? "Current deployment" : "Previous deployment";
}

function deploymentCardHeadline(
	build: DashboardBuildStatus | undefined,
): string {
	if (build?.commitMessage?.trim()) return build.commitMessage.trim();
	if (build?.commitSha) return `Commit ${shortSha(build.commitSha)}`;
	if (build?.buildId) return `Build ${shortId(build.buildId)}`;
	return "No commit message";
}

function deploymentSubtitle(
	build: DashboardBuildStatus | undefined,
	rolloutGeneration: number | undefined,
	nowMs: number,
): string | undefined {
	const timestamp =
		build?.startedAt ?? build?.queuedAt ?? build?.finishedAt ?? undefined;
	if (timestamp) return formatRelativeAge(timestamp, nowMs);
	if (rolloutGeneration !== undefined) return `Rollout ${rolloutGeneration}`;
	return undefined;
}

function matchesDeploymentLog(
	line: DashboardServiceLogLine,
	scope: {
		buildId?: string;
		allocationId?: string;
		rolloutGeneration?: number;
	},
): boolean {
	if (!scope.buildId && !scope.allocationId && !scope.rolloutGeneration) {
		return true;
	}
	if (scope.buildId && line.buildId === scope.buildId) return true;
	if (scope.allocationId && line.allocationId === scope.allocationId) {
		return true;
	}
	if (
		scope.rolloutGeneration !== undefined &&
		line.rolloutGeneration === scope.rolloutGeneration
	) {
		return true;
	}
	return false;
}

function formatDuration(ms: number): string {
	const seconds = Math.max(0, Math.round(ms / 1000));
	if (seconds < 60) return `${seconds}s`;
	return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

function deploymentMeta(
	build: DashboardBuildStatus | undefined,
	status: DashboardDeploymentStatus | undefined,
	timestamp: Date | undefined,
	nowMs: number,
): string[] {
	const parts: string[] = [];
	if (build?.commitSha) parts.push(shortSha(build.commitSha));
	if (build?.commitAuthor) parts.push(build.commitAuthor);
	if (timestamp) parts.push(formatRelativeAge(timestamp, nowMs));
	const cause = deploymentCauseLabel(status?.causeKind);
	if (cause) parts.push(cause);
	return parts;
}

function toneToHealthClass(
	tone: DeploymentCardTone,
): "building" | "healthy" | "failed" | "offline" {
	switch (tone) {
		case "running":
			return "building";
		case "active":
			return "healthy";
		case "draining":
			return "offline";
		case "failed":
			return "failed";
	}
}

function formatLogTime(date: Date): string {
	return new Intl.DateTimeFormat(undefined, {
		hour: "2-digit",
		minute: "2-digit",
		second: "2-digit",
		hour12: false,
	}).format(date);
}

function formatRelativeAge(date: Date, nowMs: number): string {
	const diffMs = date.getTime() - nowMs;
	const absSeconds = Math.round(Math.abs(diffMs) / 1000);
	const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

	if (absSeconds < 60) {
		return rtf.format(Math.round(diffMs / 1000), "second");
	}

	const absMinutes = Math.round(absSeconds / 60);
	if (absMinutes < 60) {
		return rtf.format(Math.round(diffMs / 60_000), "minute");
	}

	const absHours = Math.round(absMinutes / 60);
	if (absHours < 24) {
		return rtf.format(Math.round(diffMs / 3_600_000), "hour");
	}

	return rtf.format(Math.round(diffMs / 86_400_000), "day");
}

function formatError(error: unknown, fallback: string): string {
	if (error && typeof error === "object" && "message" in error) {
		return String((error as { message: unknown }).message);
	}
	return fallback;
}

function hydrateDeploymentRecord(
	record: DashboardDeploymentRecord,
): DashboardDeploymentRecord {
	return {
		...record,
		createdAt: hydrateDate(record.createdAt),
		build: hydrateBuildStatus(record.build),
		allocation: hydrateAllocationStatus(record.allocation),
		status: record.status
			? {
					...record.status,
					transitionedAt: hydrateDate(record.status.transitionedAt),
				}
			: undefined,
		stages:
			record.stages?.map((stage) => ({
				...stage,
				startedAt: hydrateDate(stage.startedAt),
				finishedAt: hydrateDate(stage.finishedAt),
			})) ?? record.stages,
	};
}

function hydrateBuildStatus(
	build: DashboardBuildStatus | undefined,
): DashboardBuildStatus | undefined {
	if (!build) return undefined;
	return {
		...build,
		queuedAt: hydrateDate(build.queuedAt),
		startedAt: hydrateDate(build.startedAt),
		finishedAt: hydrateDate(build.finishedAt),
		stages:
			build.stages?.map((stage) => ({
				...stage,
				startedAt: hydrateDate(stage.startedAt),
				finishedAt: hydrateDate(stage.finishedAt),
			})) ?? [],
	};
}

function hydrateAllocationStatus(
	allocation: DashboardAllocationStatus | undefined,
): DashboardAllocationStatus | undefined {
	if (!allocation) return undefined;
	return {
		...allocation,
		updatedAt: hydrateDate(allocation.updatedAt),
	};
}

function hydrateServiceLogLine(
	line: DashboardServiceLogLine,
): DashboardServiceLogLine {
	return {
		...line,
		observedAt: hydrateDate(line.observedAt),
	};
}

function hydrateDate(value: Date | string | undefined): Date | undefined {
	if (!value) return undefined;
	return value instanceof Date ? value : new Date(value);
}
