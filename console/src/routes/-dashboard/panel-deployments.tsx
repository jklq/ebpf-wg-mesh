import {
	CheckCircle2,
	Circle,
	Clock3,
	Loader2,
	RefreshCw,
	Search,
	XCircle,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";

import type {
	DashboardBuildStatus,
	DashboardDeploymentStage,
	DashboardDeploymentStageState,
	DashboardProject,
	DashboardServiceLogLine,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { fetchServiceLogs } from "./server-fns";
import { buildBadgeClass, shortId, shortSha } from "./service-utils";

type DeploymentSubview = "stages" | "logs";
type LogTypeFilter = "all" | "deploy" | "build" | "runtime";

export function PanelDeployments({
	service,
	status,
	project,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
}) {
	const [view, setView] = useState<DeploymentSubview>("stages");
	const build = status?.service.latestBuild ?? service.latestBuild;
	const allocation = status?.allocation;
	const stages = build?.stages ?? [];
	const failedStage = stages.find((stage) => stage.state === "failed");
	const showRuntimeError =
		allocation?.message && !allocation.healthy && !failedStage;

	return (
		<div className="deployments-panel">
			<DeploymentSummary
				build={build}
				service={status?.service ?? service}
				failureDetail={failedStage?.detail}
			/>

			<div className="deployment-switcher" role="tablist">
				<button
					type="button"
					className={view === "stages" ? "active" : ""}
					onClick={() => setView("stages")}
				>
					Stages
				</button>
				<button
					type="button"
					className={view === "logs" ? "active" : ""}
					onClick={() => setView("logs")}
				>
					Logs
				</button>
			</div>

			{view === "stages" && (
				<DeploymentStagesView
					stages={stages}
					runtimeError={showRuntimeError ? allocation.message : undefined}
				/>
			)}
			{view === "logs" && (
				<DeploymentLogsView
					service={status?.service ?? service}
					project={project}
					build={build}
					active={hasActiveRollout(build, allocation)}
				/>
			)}
		</div>
	);
}

function DeploymentSummary({
	build,
	service,
	failureDetail,
}: {
	build: DashboardBuildStatus | undefined;
	service: DashboardServiceRecord;
	failureDetail?: string;
}) {
	const source = service.sourceSummary?.resolvedBinding ?? service.spec?.source;
	const timestamp = build?.startedAt ?? build?.queuedAt ?? build?.finishedAt;
	return (
		<div className="deployment-summary">
			<div className="deployment-summary-top">
				<span className={`badge ${buildBadgeClass(build?.state ?? "")}`}>
					{build?.state === "running" && (
						<Loader2
							size={10}
							style={{ animation: "spin 1s linear infinite" }}
						/>
					)}
					{build?.state ?? "no build"}
				</span>
				<span className="deployment-summary-id">
					{build?.commitSha
						? shortSha(build.commitSha)
						: build?.buildId
							? shortId(build.buildId)
							: "pending"}
				</span>
				<span className="deployment-summary-time">
					{timestamp ? formatTimestamp(timestamp) : ""}
				</span>
			</div>
			<div className="deployment-meta">
				{source?.repositorySelector && <span>{source.repositorySelector}</span>}
				{source?.trackedRef && <span>{source.trackedRef}</span>}
				{build?.commitAuthor && <span>{build.commitAuthor}</span>}
			</div>
			{build?.commitMessage && (
				<p className="deployment-commit-message">{build.commitMessage}</p>
			)}
			{(build?.failureReason || failureDetail) && (
				<div className="deployment-error">
					{build?.failureReason || failureDetail}
				</div>
			)}
		</div>
	);
}

function DeploymentStagesView({
	stages,
	runtimeError,
}: {
	stages: Array<DashboardDeploymentStage>;
	runtimeError?: string;
}) {
	const failedStage = stages.find((stage) => stage.state === "failed");
	if (stages.length === 0) {
		return (
			<div className="deployment-empty">
				Deployment stages will appear when the rollout starts.
			</div>
		);
	}
	return (
		<div className="stage-list">
			{failedStage && (
				<div className="deployment-error compact">{failedStage.detail}</div>
			)}
			{runtimeError && (
				<div className="deployment-error compact">{runtimeError}</div>
			)}
			{stages.map((stage) => (
				<div
					key={stage.key || stage.label}
					className={`stage-row ${stage.state}`}
				>
					<span className={`stage-marker ${stage.state}`}>
						<StageIcon state={stage.state} />
					</span>
					<div className="stage-copy">
						<div className="stage-label">{stage.label || stage.key}</div>
						<div className="stage-detail">{stage.detail}</div>
					</div>
					<div className="stage-status">{stageStatusText(stage)}</div>
				</div>
			))}
		</div>
	);
}

function DeploymentLogsView({
	service,
	project,
	build,
	active,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject | undefined;
	build: DashboardBuildStatus | undefined;
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
			setLines(nextLines);
		} catch (cause) {
			setError(formatError(cause));
			setLines([]);
		} finally {
			setLoading(false);
		}
	}, [project, service.id, filter, build?.buildId, debouncedSearch]);

	useEffect(() => {
		void loadLogs();
	}, [loadLogs]);

	useEffect(() => {
		if (!active) return;
		const id = window.setInterval(() => void loadLogs(), 2000);
		return () => window.clearInterval(id);
	}, [active, loadLogs]);

	const sortedLines = useMemo(
		() =>
			[...lines].sort((a, b) => {
				const aTime = a.observedAt?.getTime() ?? 0;
				const bTime = b.observedAt?.getTime() ?? 0;
				return aTime - bTime || a.sequence - b.sequence;
			}),
		[lines],
	);
	const emptyText =
		search || filter !== "all"
			? "No logs match this filter."
			: "Logs will appear here as the rollout progresses.";

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
	allocation: DashboardServiceStatus["allocation"],
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

function formatDuration(ms: number): string {
	const seconds = Math.max(0, Math.round(ms / 1000));
	if (seconds < 60) return `${seconds}s`;
	return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

function formatTimestamp(date: Date): string {
	return new Intl.DateTimeFormat(undefined, {
		month: "short",
		day: "numeric",
		hour: "2-digit",
		minute: "2-digit",
	}).format(date);
}

function formatLogTime(date: Date): string {
	return new Intl.DateTimeFormat(undefined, {
		hour: "2-digit",
		minute: "2-digit",
		second: "2-digit",
		hour12: false,
	}).format(date);
}

function formatError(error: unknown): string {
	if (error && typeof error === "object" && "message" in error) {
		return String((error as { message: unknown }).message);
	}
	return "Unable to load logs.";
}
