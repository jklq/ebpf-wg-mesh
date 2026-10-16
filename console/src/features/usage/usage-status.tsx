import * as stylex from "@stylexjs/stylex";
import { TriangleAlert } from "lucide-react";
import type {
	DashboardProjectUsage,
	DashboardUsageGap,
} from "#/lib/dashboard/core/types.server";
import { dateMillis, formatDateTime, formatLogTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

/** How current the numbers are, and which parts of the range are missing. */
export function UsageFreshness({
	usage,
	loadedAtMs,
}: {
	usage: DashboardProjectUsage;
	loadedAtMs?: number;
}) {
	const retainedSince = dateMillis(usage.retainedSince);
	const expired = retainedSince > dateMillis(usage.startTime);
	return (
		<div {...stylex.props(styles.freshness)}>
			<UsageStaleNotice usage={usage} />
			<p {...stylex.props(styles.line)}>
				{usage.watermark
					? `Complete through ${formatDateTime(usage.watermark)}.`
					: "No current resource has reported usage, so recent buckets show as missing."}
				{expired &&
					` Data before ${formatDateTime(usage.retainedSince ?? "")} has passed retention.`}
				{usage.total?.estimated &&
					" Some values are estimated from partial samples."}
				{loadedAtMs !== undefined &&
					` Refreshed ${formatLogTime(new Date(loadedAtMs))}.`}
			</p>
		</div>
	);
}

/** Warns while measurements lag behind; renders nothing when current. */
export function UsageStaleNotice({ usage }: { usage: DashboardProjectUsage }) {
	if (!usage.stale) return null;
	return (
		<output {...stylex.props(styles.line, styles.stale)}>
			<TriangleAlert size={13} {...stylex.props(styles.icon)} />
			<span>
				{`Usage is behind: ${usage.staleReason || "a fleet agent has not reported"}. Recent buckets show as missing until it catches up.`}
			</span>
		</output>
	);
}

export function UsageGaps({ usage }: { usage: DashboardProjectUsage }) {
	if (usage.gaps.length === 0)
		return <p {...stylex.props(styles.empty)}>No gaps in this range.</p>;
	const names = new Map(
		usage.resources.map((resource) => [
			`${resource.kind}/${resource.resourceId}`,
			resource.name,
		]),
	);
	const label = (gap: DashboardUsageGap) =>
		gap.resourceKind === "agent"
			? "Fleet agent journal"
			: (names.get(`${gap.resourceKind}/${gap.resourceId}`) ??
				`${gap.resourceKind} ${gap.resourceId}`);
	return (
		<>
			<ul {...stylex.props(styles.gaps)}>
				{usage.gaps.map((gap) => (
					<li
						key={`${gap.resourceKind}/${gap.resourceId}/${gap.start}`}
						{...stylex.props(styles.gap)}
					>
						<strong {...stylex.props(styles.gapName)}>{label(gap)}</strong>
						<span {...stylex.props(styles.gapTime)}>
							{formatDateTime(gap.start ?? "")} –{" "}
							{formatDateTime(gap.end ?? "")}
						</span>
						<span {...stylex.props(styles.gapReason)}>{gap.reason}</span>
					</li>
				))}
			</ul>
			{usage.gapsTruncated && (
				<p {...stylex.props(styles.empty)}>
					More gaps exist than are listed. Narrow the range to see them.
				</p>
			)}
		</>
	);
}

const styles = stylex.create({
	freshness: { display: "flex", flexDirection: "column", gap: space.xs },
	line: {
		margin: "0rem",
		fontSize: "13px",
		lineHeight: "1.45",
		color: colors.muted,
	},
	stale: {
		display: "flex",
		alignItems: "flex-start",
		gap: space.sm,
		color: colors.building,
	},
	icon: { flexShrink: "0", marginTop: "3px" },
	empty: { margin: "0rem", fontSize: "13px", color: colors.muted },
	gaps: {
		margin: "0rem",
		padding: "0rem",
		listStyleType: "none",
		display: "flex",
		flexDirection: "column",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
	},
	gap: {
		display: "grid",
		gridTemplateColumns: "minmax(8rem, 1fr) auto minmax(6rem, auto)",
		alignItems: "baseline",
		gap: space.md,
		paddingInline: space.md,
		paddingBlock: space.sm,
		borderTopStyle: { default: "solid", ":first-child": "none" },
		borderTopWidth: "1px",
		borderTopColor: colors.line,
		fontSize: "13px",
	},
	gapName: { fontWeight: "600", color: colors.ink },
	gapTime: { fontFamily: fonts.mono, fontSize: "12px", color: colors.label },
	gapReason: {
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.building,
		textAlign: "right",
	},
});
