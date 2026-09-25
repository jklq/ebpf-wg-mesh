import { RefreshCw, Search } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { cn } from "#/lib/cn";
import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardProject,
	DashboardServiceLogGap,
	DashboardServiceLogLine,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { formatLogTime } from "#/lib/time";
import { errorMsg } from "#/lib/ui-classes";
import { fetchLogPage, type LogCursors } from "./log-pages";
import {
	formatError,
	hydrateServiceLogLine,
	type LogTypeFilter,
	matchesDeploymentLog,
} from "./panel-deployments-helpers";
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
			const nextLines = await fetchLogPage({
				serviceId,
				limit: 80,
				buildId,
			});
			setLines(
				nextLines.lines.map(hydrateServiceLogLine).sort((left, right) => {
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
	const [gaps, setGaps] = useState<DashboardServiceLogGap[]>([]);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<string>();
	const cursorRef = useRef<LogCursors | undefined>(undefined);
	const requestGeneration = useRef(0);
	const [hasMore, setHasMore] = useState(false);
	const [browsingHistory, setBrowsingHistory] = useState(false);

	useEffect(() => {
		const id = window.setTimeout(() => setDebouncedSearch(search.trim()), 250);
		return () => window.clearTimeout(id);
	}, [search]);

	const loadLogs = useCallback(
		async (append = false) => {
			if (!project) return;
			const generation = ++requestGeneration.current;
			setLoading(true);
			setError(undefined);
			try {
				const logType =
					filter === "all" ? undefined : logTypeFromFilter(filter);
				const nextLines = await fetchLogPage(
					{
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
					append ? cursorRef.current : undefined,
				);
				if (generation !== requestGeneration.current) return;
				cursorRef.current = nextLines;
				setHasMore(
					Boolean(nextLines.nextPageToken || nextLines.nextGapPageToken),
				);
				setBrowsingHistory(append);
				setGaps((current) =>
					append ? [...current, ...nextLines.gaps] : nextLines.gaps,
				);
				setLines((current) =>
					append
						? [...current, ...nextLines.lines.map(hydrateServiceLogLine)]
						: nextLines.lines.map(hydrateServiceLogLine),
				);
			} catch (cause) {
				if (generation === requestGeneration.current)
					setError(formatError(cause, "Unable to load logs."));
			} finally {
				if (generation === requestGeneration.current) setLoading(false);
			}
		},
		[
			project,
			service.id,
			filter,
			allocation?.allocationId,
			build?.buildId,
			debouncedSearch,
		],
	);

	useEffect(() => {
		setLines([]);
		setGaps([]);
		setHasMore(false);
		setBrowsingHistory(false);
		cursorRef.current = undefined;
		void loadLogs();
		return () => {
			requestGeneration.current++;
		};
	}, [loadLogs]);

	usePolling(loadLogs, {
		enabled: active && !browsingHistory && !loading,
		intervalMs: 2000,
	});

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
				{sortedLines.length === 0 && gaps.length === 0 && !loading && (
					<div className="px-3 pt-16 text-center text-xs text-muted">
						{emptyText}
					</div>
				)}
				{[
					...sortedLines.map((line) => ({
						time: line.observedAt?.getTime() ?? 0,
						line,
						gap: undefined as DashboardServiceLogGap | undefined,
					})),
					...gaps.map((gap) => ({
						time: gap.windowStart ? new Date(gap.windowStart).getTime() : 0,
						line: undefined as DashboardServiceLogLine | undefined,
						gap,
					})),
				]
					.sort((a, b) => a.time - b.time)
					.map(({ line, gap }) =>
						gap ? (
							<div
								key={`gap-${gap.allocationId}-${gap.buildId}-${gap.logType}-${gap.stream}-${gap.windowStart}-${gap.windowEnd}-${gap.reason}`}
								className="my-2 border-y border-building/40 bg-building/10 px-3 py-2 text-xs text-building"
							>
								{gap.windowStart && formatLogTime(new Date(gap.windowStart))} –{" "}
								{gap.windowEnd && formatLogTime(new Date(gap.windowEnd))}:{" "}
								{gap.droppedCount} lines dropped ({gap.reason})
							</div>
						) : (
							line && (
								<div
									key={
										line.lineId ??
										`${line.allocationId}-${line.buildId}-${line.stream}-${line.observedAt}-${line.sequence}`
									}
									className="px-3 py-1 text-xs"
								>
									<span className="mr-2 text-dim">
										{line.observedAt
											? formatLogTime(line.observedAt)
											: "--:--:--"}
									</span>
									{line.event ? (
										<details className="inline text-accent">
											<summary className="cursor-pointer">
												Platform event · {line.event} · {line.line}
											</summary>
											<pre className="whitespace-pre-wrap break-all">
												{JSON.stringify(line.attributes ?? {}, null, 2)}
											</pre>
										</details>
									) : (
										<span className="whitespace-pre-wrap text-ink [overflow-wrap:anywhere]">
											{line.line}
										</span>
									)}
									{line.truncated && (
										<span className="ml-2 text-building">
											(truncated at 64 KiB)
										</span>
									)}
								</div>
							)
						),
					)}
				{hasMore && (
					<button
						type="button"
						disabled={loading}
						className="m-3 border border-line px-3 py-2 text-xs text-ink"
						onClick={() => void loadLogs(true)}
					>
						{loading ? "Loading…" : "Load older logs"}
					</button>
				)}
				{loading && sortedLines.length === 0 && (
					<div className="px-3 pt-16 text-center text-xs text-muted">
						Loading logs...
					</div>
				)}
			</div>
		</div>
	);
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
