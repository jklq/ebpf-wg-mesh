import {
	ArrowLeft,
	CheckCircle2,
	Circle,
	Clock3,
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
	DashboardDeploymentStageState,
	DashboardProject,
	DashboardServiceLogLine,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { fetchServiceDeployments, fetchServiceLogs } from "./server-fns";
import { shortId, shortSha } from "./service-utils";

type LogTypeFilter = "all" | "deploy" | "build" | "runtime";

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
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
}) {
	const currentService = status?.service ?? service;
	const build = currentService.latestBuild ?? service.latestBuild;
	const allocation = status?.allocation;
	const rolloutGeneration =
		allocation?.desiredRolloutGeneration ?? currentService.rolloutGeneration;
	const activeRollout = hasActiveRollout(build, allocation);
	const [deployments, setDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [sessionDeployments, setSessionDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [deploymentsError, setDeploymentsError] = useState<string>();
	const [logTarget, setLogTarget] = useState<DeploymentLogTarget | null>(null);
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
					projectId: project.id,
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

	useEffect(() => {
		if (!activeRollout) return;
		const id = window.setInterval(() => void loadDeployments(), 5000);
		return () => window.clearInterval(id);
	}, [activeRollout, loadDeployments]);

	useEffect(() => {
		const currentDeployment = createDeploymentRecord({
			serviceId: service.id,
			build,
			allocation,
			rolloutGeneration,
			isCurrent: true,
		});
		const previousDeployment = lastObservedDeployment.current;

		if (
			previousDeployment &&
			!isSameDeploymentRecord(previousDeployment, currentDeployment) &&
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

		lastObservedDeployment.current = currentDeployment;
	}, [service.id, build, allocation, rolloutGeneration]);

	const previousDeployments = useMemo(
		() =>
			mergeDeploymentRecords([...sessionDeployments, ...deployments]).filter(
				(entry) =>
					!isCurrentDeployment(entry, build?.buildId, rolloutGeneration) &&
					shouldRenderDeploymentHistoryEntry(entry, currentService),
			),
		[
			deployments,
			sessionDeployments,
			build?.buildId,
			rolloutGeneration,
			currentService,
		],
	);

	return (
		<div className="deployments-panel">
			<div className="deployments-list">
				<CurrentDeploymentCard
					build={build}
					allocation={allocation}
					logsEnabled={Boolean(project)}
					nowMs={nowMs}
					onOpenLogs={() =>
						setLogTarget({
							id: build?.buildId ?? `${service.id}-current`,
							title: deploymentTitle(build, true),
							subtitle: deploymentSubtitle(build, rolloutGeneration, nowMs),
							build,
							allocation,
							rolloutGeneration,
							active: activeRollout,
						})
					}
				/>

				{previousDeployments.length > 0 && (
					<section className="deployment-history">
						<div className="deployment-section-heading">Previous</div>
						<div className="deployment-history-list">
							{previousDeployments.map((entry) => (
								<DeploymentHistoryRow
									key={deploymentRecordKey(entry)}
									build={entry.build}
									allocation={entry.allocation}
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
											active: hasActiveRollout(entry.build, entry.allocation),
										})
									}
								/>
							))}
						</div>
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

function CurrentDeploymentCard({
	build,
	allocation,
	logsEnabled,
	nowMs,
	onOpenLogs,
}: {
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	logsEnabled: boolean;
	nowMs: number;
	onOpenLogs: () => void;
}) {
	const stages = build?.stages ?? [];
	const active = hasActiveRollout(build, allocation);
	const timestamp =
		build?.startedAt ??
		build?.queuedAt ??
		build?.finishedAt ??
		allocation?.updatedAt;
	const tone = getDeploymentCardTone({
		build,
		allocation,
		active,
		isCurrent: true,
	});
	const meta = deploymentMeta(build);

	return (
		<section className={`deployment-shell deployment-shell-current ${tone ? `tone-${tone}` : ""}`}>
			<div className="deployment-current-head">
				<div className="deployment-current-copy">
					<p className="deployment-current-message">
						{deploymentCardHeadline(build)}
					</p>
					<div className="deployment-current-meta">
						{meta.map((entry, index) => (
							<span key={`${entry}-${index}`}>
								{index > 0 && <span className="deployment-inline-dot" />}
								{entry}
							</span>
						))}
						{timestamp && meta.length > 0 && (
							<span className="deployment-inline-dot" />
						)}
						{timestamp && (
							<span className="deployment-current-time">
								{formatRelativeAge(timestamp, nowMs)}
							</span>
						)}
					</div>
				</div>
				<button
					type="button"
					className="deployment-view-logs"
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					Logs
				</button>
			</div>

			{stages.length > 0 ? (
				<ol className="stage-list">
					{stages.map((stage) => {
						// While the deployment is still active, already-succeeded stages use
						// the building colour so the whole list reads as one amber tone.
						const markerState =
							tone === "running" && stage.state === "succeeded"
								? "building-done"
								: stage.state;
						return (
							<li key={stage.key || stage.label} className={`stage-row ${stage.state}`}>
								<span className={`stage-marker ${markerState}`}>
									<StageIcon state={stage.state} />
								</span>
								<div className="stage-copy">
									<div className="stage-label">{stage.label || stage.key}</div>
									{stage.detail && <div className="stage-detail">{stage.detail}</div>}
								</div>
								<div className="stage-status">{stageStatusText(stage)}</div>
							</li>
						);
					})}
				</ol>
			) : (
				<div className="deployment-empty-state">
					No stage data has been reported for this deployment yet.
				</div>
			)}
		</section>
	);
}

function DeploymentHistoryRow({
	build,
	allocation,
	logsEnabled,
	nowMs,
	onOpenLogs,
}: {
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	logsEnabled: boolean;
	nowMs: number;
	onOpenLogs: () => void;
}) {
	const tone =
		getDeploymentCardTone({
			build,
			allocation,
			active: hasActiveRollout(build, allocation),
			isCurrent: false,
		}) ?? "failed";
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
				{build?.commitSha && <span className="mono">{shortSha(build.commitSha)}</span>}
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
					projectId: project.id,
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

	useEffect(() => {
		if (!active) return;
		const id = window.setInterval(() => void loadLogs(), 2000);
		return () => window.clearInterval(id);
	}, [active, loadLogs]);

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

function StageIcon({ state }: { state: DashboardDeploymentStageState }) {
	switch (state) {
		case "running":
			return (
				<Loader2 size={13} style={{ animation: "spin 1s linear infinite" }} />
			);
		case "succeeded":
			return <CheckCircle2 size={13} />;
		case "failed":
			return <XCircle size={13} />;
		case "pending":
			return <Clock3 size={13} />;
		default:
			return <Circle size={13} />;
	}
}

function getDeploymentCardTone({
	build,
	allocation,
	active,
	isCurrent,
}: {
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	active: boolean;
	isCurrent: boolean;
}): "running" | "succeeded" | "failed" | undefined {
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
		return "succeeded";
	}

	if (!isCurrent && build?.state === "succeeded") {
		return "succeeded";
	}

	return undefined;
}

function stageStatusText(stage: DashboardDeploymentStage): string {
	if (stage.startedAt && stage.finishedAt) {
		return formatDuration(
			stage.finishedAt.getTime() - stage.startedAt.getTime(),
		);
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

function hasActiveRollout(
	build: DashboardBuildStatus | undefined,
	allocation: DashboardAllocationStatus | undefined,
): boolean {
	const activeStage = build?.stages?.some(
		(stage) => stage.state === "running" || stage.state === "pending",
	);
	const rolloutMismatch =
		allocation &&
		allocation.desiredRolloutGeneration !== allocation.appliedRolloutGeneration;
	return Boolean(
		activeStage ||
			rolloutMismatch ||
			build?.state === "queued" ||
			build?.state === "running",
	);
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

function isCurrentDeployment(
	entry: DashboardDeploymentRecord,
	currentBuildId?: string,
	currentRolloutGeneration?: number,
): boolean {
	if (entry.isCurrent) return true;
	if (currentBuildId && entry.build?.buildId === currentBuildId) return true;
	return (
		currentRolloutGeneration !== undefined &&
		entry.rolloutGeneration === currentRolloutGeneration
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
}: {
	serviceId: string;
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	rolloutGeneration?: number;
	isCurrent: boolean;
}): DashboardDeploymentRecord {
	return {
		id: serviceId,
		rolloutGeneration: rolloutGeneration ?? 0,
		createdAt:
			build?.startedAt ??
			build?.queuedAt ??
			build?.finishedAt ??
			allocation?.updatedAt,
		build,
		allocation,
		isCurrent,
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

function deploymentMeta(build: DashboardBuildStatus | undefined): string[] {
	const parts: string[] = [];
	if (build?.commitSha) parts.push(shortSha(build.commitSha));
	if (build?.commitAuthor) parts.push(build.commitAuthor);
	if (build?.state && build.state !== "unspecified") parts.push(build.state);
	return parts;
}

function toneToHealthClass(
	tone: "running" | "succeeded" | "failed",
): "building" | "healthy" | "failed" {
	switch (tone) {
		case "running":
			return "building";
		case "succeeded":
			return "healthy";
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
