import { RefreshCw, Search } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";

import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardProject,
	DashboardServiceLogLine,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import {
	formatError,
	formatLogTime,
	hydrateServiceLogLine,
	type LogTypeFilter,
	matchesDeploymentLog,
} from "./panel-deployments-helpers";
import { fetchServiceLogs } from "./server-fns";
import { usePolling } from "./use-polling";

export function useInlineDeploymentLogs({
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

export function DeploymentLogsView({
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
