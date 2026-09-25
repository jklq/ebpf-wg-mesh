import { useCallback, useEffect, useState } from "react";
import { cn } from "#/lib/cn";
import type {
	DashboardAllocationStatus,
	DashboardServiceLogLine,
} from "#/lib/dashboard/core/types.server";
import { errorMsg } from "#/lib/ui-classes";
import {
	crashCauseForAllocation,
	crashCauseLabel,
	formatCrashSummary,
	hasCrashEvidence,
} from "./crash-evidence";
import { hydrateServiceLogLine } from "./panel-deployments-helpers";
import { fetchServiceLogs } from "./server-fns";
import { shortId } from "./service-utils";

const CRASH_LOG_TAIL_LIMIT = 30;
// Crash status reaches the dashboard before the agent's buffered runtime batch
// reaches ClickHouse, so an empty tail is retried a few times before
// concluding that no logs were retained.
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
		<div className="flex flex-col gap-2 border-t border-dashed border-[rgba(80,76,71,0.55)] px-7 py-3 max-[900px]:px-4">
			<div className="font-condensed text-[11px] font-bold uppercase tracking-[0.09em] text-muted">
				Crash evidence
			</div>
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
		<div className="overflow-hidden border border-[rgba(80,76,71,0.55)] bg-[#10100f]">
			<div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-2.5 py-2">
				<span className="font-mono text-[11px] text-dim">
					{shortId(allocation.allocationId)} on {shortId(allocation.agentId)}
				</span>
				<span
					className={cn(
						"font-condensed text-[11px] font-bold uppercase tracking-[0.08em]",
						cause === "probe-not-ready" ? "text-building" : "text-failed",
					)}
				>
					{crashCauseLabel(cause)}
				</span>
				{restart?.crashLoop && (
					<span className="border border-[rgba(184,66,66,0.4)] bg-[rgba(184,66,66,0.12)] px-1.5 py-0.5 font-condensed text-[10px] font-bold uppercase tracking-[0.08em] text-failed">
						Crash loop
					</span>
				)}
				<span className="w-full font-mono text-[11px] leading-[1.5] text-muted">
					{formatCrashSummary(allocation)}
				</span>
				{(restart?.message || allocation.message) && (
					<span className="w-full text-[11px] leading-[1.5] text-dim">
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

// Revision of the crash evidence for one allocation. A second crash reuses
// the same allocation ID, so the log tail keys off this revision to refetch
// instead of showing the earlier crash tail.
function crashRevision(allocation: DashboardAllocationStatus): string {
	const restart = allocation.restart;
	if (!restart) return "no-restart";
	return [
		restart.restartCount,
		restart.lastRestartAt?.getTime() ?? 0,
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
		<dl className="grid w-full grid-cols-[repeat(auto-fit,minmax(120px,1fr))] gap-x-3 gap-y-1 pt-1">
			{facts.map(([label, value]) => (
				<div key={label} className="flex items-baseline gap-1.5">
					<dt className="font-condensed text-[10px] font-bold uppercase tracking-[0.08em] text-dim">
						{label}
					</dt>
					<dd className="m-0 font-mono text-[11px] text-muted">{value}</dd>
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
				next.lines.map(hydrateServiceLogLine).sort((left, right) => {
					const leftTime = left.observedAt?.getTime() ?? 0;
					const rightTime = right.observedAt?.getTime() ?? 0;
					return leftTime - rightTime || left.sequence - right.sequence;
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
		<div className="border-t border-[rgba(80,76,71,0.45)]">
			<button
				type="button"
				className="flex w-full cursor-pointer items-center justify-between border-0 bg-transparent px-2.5 py-1.5 font-condensed text-[10px] font-bold uppercase tracking-[0.08em] text-dim hover:text-ink"
				aria-expanded={open}
				onClick={() => setOpen((value) => !value)}
			>
				<span>Last runtime logs</span>
				<span aria-hidden>{open ? "▾" : "▸"}</span>
			</button>
			{open && (
				<div className="px-2.5 pb-2.5">
					{error && (
						<div className={cn(errorMsg, "px-[9px] py-[7px] text-[11px]")}>
							{error}
						</div>
					)}
					{awaitingLogs && (
						<div className="py-2 font-mono text-[11px] text-dim">
							Loading crash logs…
						</div>
					)}
					{!error && !loading && lines.length === 0 && !awaitingLogs && (
						<div className="py-2 font-mono text-[11px] text-dim">
							No runtime logs retained for this allocation.
						</div>
					)}
					{lines.length > 0 && (
						<div
							role="log"
							aria-label="Crash log tail"
							className="max-h-40 overflow-auto bg-black/40 font-mono text-[11px] leading-[1.6]"
						>
							{lines.map((line) => (
								<div
									key={`${line.observedAt?.toISOString() ?? "t"}-${line.sequence}-${line.line}`}
									className="px-2 py-[2px] whitespace-pre-wrap text-dim [overflow-wrap:anywhere]"
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
