import * as stylex from "@stylexjs/stylex";
import { noticeStyles } from "#/components/ui/notice";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { RefreshCw, Search } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
	fetchLogPage,
	type LogCursors,
} from "#/features/dashboard/service-panel/deployments/log-pages";
import {
	type LogTypeFilter,
	matchesDeploymentLog,
} from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import { usePolling } from "#/hooks/use-polling";
import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardProject,
	DashboardServiceLogGap,
	DashboardServiceLogLine,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/errors";
import { compareIntegers } from "#/lib/platform-json";
import { dateMillis, formatLogTime } from "#/lib/time";

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
				nextLines.lines.sort((left, right) => {
					const leftTime = dateMillis(left.observedAt) ?? 0;
					const rightTime = dateMillis(right.observedAt) ?? 0;
					return (
						leftTime - rightTime ||
						compareIntegers(left.sequence, right.sequence)
					);
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
	rolloutGeneration?: string;
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
					append ? [...current, ...nextLines.lines] : nextLines.lines,
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
				const aTime = dateMillis(a.observedAt) ?? 0;
				const bTime = dateMillis(b.observedAt) ?? 0;
				return aTime - bTime || compareIntegers(a.sequence, b.sequence);
			}),
		[filteredLines],
	);

	const emptyText =
		search || filter !== "all"
			? "No logs match this filter."
			: "Logs will appear here as this deployment progresses.";

	return (
		<div {...stylex.props(styles.panel)}>
			<div {...stylex.props(styles.toolbar)}>
				<label {...stylex.props(styles.searchLabel)}>
					<Search size={13} />
					<input
						{...stylex.props(styles.searchInput)}
						value={search}
						onChange={(event) => setSearch(event.target.value)}
						placeholder="Search logs"
					/>
				</label>
				<button
					type="button"
					{...stylex.props(styles.refreshButton)}
					onClick={() => void loadLogs()}
					disabled={loading}
					aria-label="Refresh logs"
					title="Refresh logs"
				>
					<RefreshCw
						size={13}
						{...stylex.props(loading ? styles.refreshing : undefined)}
					/>
				</button>
			</div>

			<div {...stylex.props(styles.filters)}>
				{(["all", "deploy", "build", "runtime"] as const).map((type) => (
					<button
						key={type}
						type="button"
						{...stylex.props([
							styles.filterButton,
							filter === type ? styles.selectedFilter : styles.idleFilter,
						])}
						onClick={() => setFilter(type)}
					>
						{type}
					</button>
				))}
			</div>

			{error && (
				<div {...stylex.props([noticeStyles.error, styles.errorMessage])}>
					{error}
				</div>
			)}

			<div {...stylex.props(styles.logViewport)}>
				{sortedLines.length === 0 && gaps.length === 0 && !loading && (
					<div {...stylex.props(styles.emptyState)}>{emptyText}</div>
				)}
				{[
					...sortedLines.map((line) => ({
						time: dateMillis(line.observedAt) ?? 0,
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
								{...stylex.props(styles.logGap)}
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
									{...stylex.props(styles.logRow)}
								>
									<span {...stylex.props(styles.timestamp)}>
										{line.observedAt
											? formatLogTime(line.observedAt)
											: "--:--:--"}
									</span>
									{line.event ? (
										<details {...stylex.props(styles.eventDetails)}>
											<summary {...stylex.props(styles.eventSummary)}>
												Platform event · {line.event} · {line.line}
											</summary>
											<pre {...stylex.props(styles.eventAttributes)}>
												{JSON.stringify(line.attributes ?? {}, null, 2)}
											</pre>
										</details>
									) : (
										<span {...stylex.props(styles.logText)}>{line.line}</span>
									)}
									{line.truncated && (
										<span {...stylex.props(styles.truncationNote)}>
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
						{...stylex.props(styles.loadOlderButton)}
						onClick={() => void loadLogs(true)}
					>
						{loading ? "Loading…" : "Load older logs"}
					</button>
				)}
				{loading && sortedLines.length === 0 && (
					<div {...stylex.props(styles.emptyState)}>Loading logs...</div>
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

const styles = stylex.create({
	panel: {
		display: "flex",
		minHeight: "0rem",
		flex: "1",
		flexDirection: "column",
		gap: "9px",
		overflow: "hidden",
		padding: space.lg,
	},
	toolbar: { display: "flex", alignItems: "center", gap: space.sm },
	searchLabel: {
		display: "flex",
		flex: "1",
		alignItems: "center",
		gap: "7px",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
		paddingInline: "9px",
		color: colors.dim,
	},
	searchInput: {
		minWidth: "0rem",
		flex: "1",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		paddingBlock: space.sm,
		fontFamily: fonts.mono,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.ink,
		outlineStyle: "none",
	},
	refreshButton: {
		display: "inline-flex",
		width: "31px",
		height: "31px",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: {
			default: colors.canvas,
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
	},
	refreshing: { animation: `${spin} 1s linear infinite` },
	filters: {
		display: "flex",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
	},
	filterButton: {
		minHeight: "2rem",
		flex: "1",
		cursor: "pointer",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.sm,
		paddingBlock: "0.375rem",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.09em",
	},
	selectedFilter: {
		backgroundColor: colors.surfaceRaised,
		color: colors.accent,
	},
	idleFilter: {
		backgroundColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		color: colors.ink,
	},
	errorMessage: { paddingInline: "9px", paddingBlock: "7px", fontSize: "11px" },
	logViewport: {
		minHeight: "220px",
		flex: "1",
		overflow: "auto",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "#10100f",
		paddingBlock: space.sm,
		fontFamily: fonts.mono,
	},
	emptyState: {
		paddingInline: space.md,
		paddingTop: "4rem",
		textAlign: "center",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	logGap: {
		marginBlock: space.sm,
		borderBlockStyle: "solid",
		borderBlockWidth: "1px",
		borderColor: `color-mix(in oklab, ${colors.building} 40%, transparent)`,
		backgroundColor: `color-mix(in oklab, ${colors.building} 10%, transparent)`,
		paddingInline: space.md,
		paddingBlock: space.sm,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.building,
	},
	logRow: {
		paddingInline: space.md,
		paddingBlock: space.xs,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
	},
	timestamp: { marginRight: space.sm, color: colors.dim },
	eventDetails: { display: "inline", color: colors.accent },
	eventSummary: { cursor: "pointer" },
	eventAttributes: { whiteSpace: "pre-wrap", wordBreak: "break-all" },
	logText: {
		whiteSpace: "pre-wrap",
		color: colors.ink,
		overflowWrap: "anywhere",
	},
	truncationNote: { marginLeft: space.sm, color: colors.building },
	loadOlderButton: {
		margin: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.md,
		paddingBlock: space.sm,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.ink,
	},
});
