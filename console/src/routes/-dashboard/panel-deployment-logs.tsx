import { RefreshCw, Search } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";

import { cn } from "#/lib/cn";
import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardProject,
	DashboardServiceLogLine,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { errorMsg } from "#/lib/ui-classes";
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
			const logType = filter === "all" ? undefined : logTypeFromFilter(filter);
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
		<div className="flex min-h-0 flex-1 flex-col gap-[9px] overflow-hidden p-4">
			<div className="flex items-center gap-2">
				<label className="flex flex-1 items-center gap-[7px] border border-line bg-canvas px-[9px] text-dim">
					<Search size={13} />
					<input
						className="min-w-0 flex-1 border-0 bg-transparent py-2 font-mono text-xs text-ink outline-none"
						value={search}
						onChange={(event) => setSearch(event.target.value)}
						placeholder="Search logs"
					/>
				</label>
				<button
					type="button"
					className="inline-flex size-[31px] cursor-pointer items-center justify-center border border-line bg-canvas text-muted hover:bg-surface-hover hover:text-ink"
					onClick={() => void loadLogs()}
					disabled={loading}
					aria-label="Refresh logs"
					title="Refresh logs"
				>
					<RefreshCw size={13} className={loading ? "animate-spin" : ""} />
				</button>
			</div>

			<div className="flex border border-line bg-canvas">
				{(["all", "deploy", "build", "runtime"] as const).map((type) => (
					<button
						key={type}
						type="button"
						className={cn(
							"min-h-8 flex-1 cursor-pointer border border-line px-2 py-1.5 font-condensed text-[11px] font-bold uppercase tracking-[0.09em]",
							filter === type
								? "bg-surface-raised text-accent"
								: "bg-transparent text-ink hover:bg-surface-hover",
						)}
						onClick={() => setFilter(type)}
					>
						{type}
					</button>
				))}
			</div>

			{error && (
				<div className={cn(errorMsg, "px-[9px] py-[7px] text-[11px]")}>
					{error}
				</div>
			)}

			<div className="min-h-[220px] flex-1 overflow-auto border border-line bg-[#10100f] py-2 font-mono">
				{sortedLines.length === 0 && !loading && (
					<div className="px-3 pt-16 text-center text-xs text-muted">
						{emptyText}
					</div>
				)}
				{sortedLines.map((line) => (
					<div
						key={`${line.observedAt?.toISOString() ?? "t"}-${line.sequence}-${line.line}`}
						className="grid grid-cols-[max-content_minmax(0,1fr)] items-start gap-2 px-[9px] py-[3px] text-[11px] leading-[1.45] text-muted max-[900px]:grid-cols-1 max-[900px]:gap-0.5"
					>
						<div className="inline-flex items-baseline gap-1.5 whitespace-nowrap max-[900px]:flex-wrap">
							<span className="text-dim">
								{line.observedAt ? formatLogTime(line.observedAt) : "--:--:--"}
							</span>
							<span
								className={cn(
									"log-badge text-[9px] tracking-[0.06em] uppercase",
									line.logType ?? "unspecified",
									logBadgeToneClass(line.logType),
								)}
							>
								{line.logType?.replace("SERVICE_LOG_TYPE_", "").toLowerCase() ??
									"log"}
							</span>
						</div>
						<span className="whitespace-pre-wrap text-ink [overflow-wrap:anywhere]">
							{line.line}
						</span>
					</div>
				))}
				{loading && sortedLines.length === 0 && (
					<div className="px-3 pt-16 text-center text-xs text-muted">
						Loading logs...
					</div>
				)}
			</div>
		</div>
	);
}

function logBadgeToneClass(logType: string | undefined): string {
	switch (logType) {
		case "SERVICE_LOG_TYPE_RUNTIME":
			return "text-healthy";
		case "SERVICE_LOG_TYPE_BUILD":
			return "text-building";
		case "SERVICE_LOG_TYPE_HTTP":
		case "SERVICE_LOG_TYPE_NETWORK":
			return "text-muted";
		default:
			return "text-accent";
	}
}

function logTypeFromFilter(
	filter: Exclude<LogTypeFilter, "all">,
):
	| "SERVICE_LOG_TYPE_RUNTIME"
	| "SERVICE_LOG_TYPE_BUILD"
	| "SERVICE_LOG_TYPE_DEPLOY" {
	switch (filter) {
		case "runtime":
			return "SERVICE_LOG_TYPE_RUNTIME";
		case "build":
			return "SERVICE_LOG_TYPE_BUILD";
		case "deploy":
			return "SERVICE_LOG_TYPE_DEPLOY";
	}
}
