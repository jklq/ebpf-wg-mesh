import * as stylex from "@stylexjs/stylex";
import { useCallback, useEffect, useState } from "react";
import { noticeStyles } from "#/components/ui/notice";
import {
	crashCauseForAllocation,
	crashCauseLabel,
	formatCrashSummary,
	hasCrashEvidence,
} from "#/features/dashboard/service-panel/deployments/crash-evidence";
import { shortId } from "#/features/dashboard/shared/service-utils";
import type {
	DashboardAllocationStatus,
	DashboardServiceLogLine,
} from "#/lib/dashboard/core/types.server";
import { fetchServiceLogs } from "#/lib/dashboard/server-functions";
import { compareIntegers } from "#/lib/platform-json";
import { dateMillis } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const CRASH_LOG_TAIL_LIMIT = 30;
// Crash status arrives before the buffered runtime batch reaches ClickHouse, so an empty tail is retried before concluding no logs were retained.
const CRASH_LOG_EMPTY_RETRIES = 4;
const CRASH_LOG_EMPTY_RETRY_DELAY_MS = 1500;

export function AllocationCrashEvidence({
	serviceId,
	allocations,
	logsEnabled,
}: {
	serviceId: string;
	allocations: Array<DashboardAllocationStatus>;
	logsEnabled: boolean;
}) {
	const crashed = allocations.filter(hasCrashEvidence);
	if (crashed.length === 0) return null;
	return (
		<div {...stylex.props(styles.evidence)}>
			<div {...stylex.props(styles.title)}>Crash evidence</div>
			{crashed.map((allocation) => (
				<AllocationCrashCard
					key={allocation.allocationId}
					serviceId={serviceId}
					allocation={allocation}
					logsEnabled={logsEnabled}
				/>
			))}
		</div>
	);
}

function AllocationCrashCard({
	serviceId,
	allocation,
	logsEnabled,
}: {
	serviceId: string;
	allocation: DashboardAllocationStatus;
	logsEnabled: boolean;
}) {
	const cause = crashCauseForAllocation(allocation);
	const restart = allocation.restart;
	return (
		<div {...stylex.props(styles.crashCard)}>
			<div {...stylex.props(styles.crashHeader)}>
				<span {...stylex.props(styles.allocationLabel)}>
					{shortId(allocation.allocationId)} on {shortId(allocation.agentId)}
				</span>
				<span
					{...stylex.props([
						styles.causeBadge,
						cause === "probe-not-ready"
							? styles.failedCause
							: styles.mutedCause,
					])}
				>
					{crashCauseLabel(cause)}
				</span>
				{restart?.crashLoop && (
					<span {...stylex.props(styles.crashLoopBadge)}>Crash loop</span>
				)}
				<span {...stylex.props(styles.summary)}>
					{formatCrashSummary(allocation)}
				</span>
				{(restart?.message || allocation.message) && (
					<span {...stylex.props(styles.message)}>
						{restart?.message || allocation.message}
					</span>
				)}
				<CrashFacts allocation={allocation} />
			</div>
			<CrashLogTail
				key={`${serviceId}:${allocation.allocationId}:${logsEnabled}:${crashRevision(allocation)}`}
				serviceId={serviceId}
				allocationId={allocation.allocationId}
				enabled={logsEnabled}
			/>
		</div>
	);
}

// Crash evidence revision per allocation. A second crash reuses the allocation ID, so the tail keys off this revision to refetch.
function crashRevision(allocation: DashboardAllocationStatus): string {
	const restart = allocation.restart;
	if (!restart) return "no-restart";
	return [
		restart.restartCount,
		dateMillis(restart.lastRestartAt) ?? 0,
		restart.lastCause,
		restart.lastExitCode,
		restart.lastSignal,
	].join(":");
}

function CrashFacts({ allocation }: { allocation: DashboardAllocationStatus }) {
	const restart = allocation.restart;
	if (!restart) return null;
	const facts: Array<[string, string]> = [];
	facts.push(["Restarts", String(restart.restartCount)]);
	facts.push(["Exit code", String(restart.lastExitCode)]);
	facts.push(["Signal", String(restart.lastSignal)]);
	facts.push([
		"Last restart",
		restart.lastRestartAt ? restart.lastRestartAt.toLocaleString() : "—",
	]);
	return (
		<dl {...stylex.props(styles.facts)}>
			{facts.map(([label, value]) => (
				<div key={label} {...stylex.props(styles.fact)}>
					<dt {...stylex.props(styles.factLabel)}>{label}</dt>
					<dd {...stylex.props(styles.factValue)}>{value}</dd>
				</div>
			))}
		</dl>
	);
}

function CrashLogTail({
	serviceId,
	allocationId,
	enabled,
}: {
	serviceId: string;
	allocationId: string;
	enabled: boolean;
}) {
	const [lines, setLines] = useState<Array<DashboardServiceLogLine>>([]);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<string>();
	const [open, setOpen] = useState(true);
	const [emptyRetries, setEmptyRetries] = useState(0);

	const load = useCallback(async () => {
		if (!enabled) return;
		setLoading(true);
		setError(undefined);
		try {
			const next = await fetchServiceLogs({
				data: {
					serviceId,
					allocationId,
					limit: CRASH_LOG_TAIL_LIMIT,
					logType: "SERVICE_LOG_TYPE_RUNTIME",
				},
			});
			setLines(
				next.lines.sort((left, right) => {
					const leftTime = dateMillis(left.observedAt) ?? 0;
					const rightTime = dateMillis(right.observedAt) ?? 0;
					return (
						leftTime - rightTime ||
						compareIntegers(left.sequence, right.sequence)
					);
				}),
			);
		} catch (cause) {
			setError(
				cause && typeof cause === "object" && "message" in cause
					? String((cause as { message: unknown }).message)
					: "Unable to load crash logs.",
			);
			setLines([]);
		} finally {
			setLoading(false);
		}
	}, [enabled, serviceId, allocationId]);

	useEffect(() => {
		void load();
	}, [load]);

	useEffect(() => {
		if (
			!enabled ||
			loading ||
			error ||
			lines.length > 0 ||
			emptyRetries >= CRASH_LOG_EMPTY_RETRIES
		) {
			return;
		}
		const timer = setTimeout(() => {
			setEmptyRetries((count) => count + 1);
			void load();
		}, CRASH_LOG_EMPTY_RETRY_DELAY_MS);
		return () => clearTimeout(timer);
	}, [enabled, loading, error, lines.length, emptyRetries, load]);

	if (!enabled) return null;
	const awaitingLogs =
		!error && lines.length === 0 && emptyRetries < CRASH_LOG_EMPTY_RETRIES;
	return (
		<div {...stylex.props(styles.logsDisclosure)}>
			<button
				type="button"
				{...stylex.props(styles.logsToggle)}
				aria-expanded={open}
				onClick={() => setOpen((value) => !value)}
			>
				<span>Last runtime logs</span>
				<span aria-hidden>{open ? "▾" : "▸"}</span>
			</button>
			{open && (
				<div {...stylex.props(styles.logsBody)}>
					{error && (
						<div {...stylex.props([noticeStyles.error, styles.logsError])}>
							{error}
						</div>
					)}
					{awaitingLogs && (
						<div {...stylex.props(styles.logsLoading)}>Loading crash logs…</div>
					)}
					{!error && !loading && lines.length === 0 && !awaitingLogs && (
						<div {...stylex.props(styles.logsLoading)}>
							No runtime logs retained for this allocation.
						</div>
					)}
					{lines.length > 0 && (
						<div
							role="log"
							aria-label="Crash log tail"
							{...stylex.props(styles.logTail)}
						>
							{lines.map((line) => (
								<div
									key={`${line.observedAt ?? "t"}-${line.sequence}-${line.line}`}
									{...stylex.props(styles.logLine)}
								>
									{line.line}
								</div>
							))}
						</div>
					)}
				</div>
			)}
		</div>
	);
}

const styles = stylex.create({
	evidence: {
		display: "flex",
		flexDirection: "column",
		gap: space.sm,
		borderTopStyle: "dashed",
		borderTopWidth: "1px",
		borderStyle: "dashed",
		borderColor: "rgba(80,76,71,0.55)",
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBlock: space.md,
	},
	title: {
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.09em",
		color: colors.muted,
	},
	crashCard: {
		overflow: "hidden",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(80,76,71,0.55)",
		backgroundColor: "#10100f",
	},
	crashHeader: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		columnGap: space.md,
		rowGap: space.xs,
		paddingInline: "0.625rem",
		paddingBlock: space.sm,
	},
	allocationLabel: {
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.dim,
	},
	causeBadge: {
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.08em",
	},
	failedCause: { color: colors.building },
	mutedCause: { color: colors.failed },
	crashLoopBadge: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(184,66,66,0.4)",
		backgroundColor: "rgba(184,66,66,0.12)",
		paddingInline: "0.375rem",
		paddingBlock: "0.125rem",
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.08em",
		color: colors.failed,
	},
	summary: {
		width: "100%",
		fontFamily: fonts.mono,
		fontSize: "11px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	message: {
		width: "100%",
		fontSize: "11px",
		lineHeight: "1.5",
		color: colors.dim,
	},
	facts: {
		display: "grid",
		width: "100%",
		gridTemplateColumns: "repeat(auto-fit,minmax(120px,1fr))",
		columnGap: space.md,
		rowGap: space.xs,
		paddingTop: space.xs,
	},
	fact: { display: "flex", alignItems: "baseline", gap: "0.375rem" },
	factLabel: {
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.08em",
		color: colors.dim,
	},
	factValue: {
		margin: "0rem",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.muted,
	},
	logsDisclosure: {
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: "rgba(80,76,71,0.45)",
	},
	logsToggle: {
		display: "flex",
		width: "100%",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "space-between",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		paddingInline: "0.625rem",
		paddingBlock: "0.375rem",
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.08em",
		color: {
			default: colors.dim,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
	},
	logsBody: { paddingInline: "0.625rem", paddingBottom: "0.625rem" },
	logsError: { paddingInline: "9px", paddingBlock: "7px", fontSize: "11px" },
	logsLoading: {
		paddingBlock: space.sm,
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.dim,
	},
	logTail: {
		maxHeight: "10rem",
		overflow: "auto",
		backgroundColor: "color-mix(in oklab, #000 40%, transparent)",
		fontFamily: fonts.mono,
		fontSize: "11px",
		lineHeight: "1.6",
	},
	logLine: {
		paddingInline: space.sm,
		paddingBlock: "2px",
		whiteSpace: "pre-wrap",
		color: colors.dim,
		overflowWrap: "anywhere",
	},
});
