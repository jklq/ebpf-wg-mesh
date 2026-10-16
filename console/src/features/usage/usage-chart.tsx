import * as stylex from "@stylexjs/stylex";
import {
	type BucketState,
	bucketState,
	isMissing,
	type UsageMetric,
	usageMetrics,
} from "#/features/usage/usage-model";
import type { DashboardProjectUsage } from "#/lib/dashboard/core/types.server";
import { formatDateTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const HEIGHT = 100;
const stateLabels: Record<BucketState, string> = {
	complete: "complete",
	incomplete: "incomplete, still receiving usage",
	pending: "no data yet",
	gap: "no data: measurement gap",
	expired: "no data: past retention",
};

/**
 * One bar per bucket in SVG user units: x is the bucket index, y the share of
 * the tallest value. Incomplete buckets are dimmed; missing buckets are a
 * shaded band, never a zero bar.
 */
export function UsageChart({
	usage,
	metric,
}: {
	usage: DashboardProjectUsage;
	metric: UsageMetric;
}) {
	const { label, value, format } = usageMetrics[metric];
	const bars = usage.buckets.map((bucket) => {
		const state = bucketState(bucket, usage);
		return {
			bucket,
			state,
			value:
				isMissing(state) || !bucket.usage ? undefined : value(bucket.usage),
		};
	});
	const max = Math.max(0, ...bars.map((bar) => bar.value ?? 0));
	const count = Math.max(bars.length, 1);

	return (
		<figure {...stylex.props(styles.figure)}>
			<div {...stylex.props(styles.axisLabel)}>
				{max > 0 ? format(max) : format(0)}
			</div>
			<svg
				viewBox={`0 0 ${count} ${HEIGHT}`}
				preserveAspectRatio="none"
				role="img"
				aria-label={`${label} per bucket`}
				{...stylex.props(styles.plot)}
			>
				{bars.map(({ bucket, state, value }, index) => {
					const missing = value === undefined;
					const height = missing || max === 0 ? 0 : (value / max) * HEIGHT;
					const gapNote =
						!missing && (bucket.usage?.gapMilliseconds ?? "0") !== "0"
							? ", includes gaps"
							: "";
					return (
						<g key={bucket.start ?? index}>
							{missing && (
								<rect
									x={index}
									y={0}
									width={1}
									height={HEIGHT}
									{...stylex.props(styles.missing)}
								/>
							)}
							{!missing && height > 0 && (
								<rect
									x={index + 0.1}
									y={HEIGHT - height}
									width={0.8}
									height={height}
									{...stylex.props(
										styles.bar,
										state === "incomplete" && styles.incomplete,
									)}
								/>
							)}
							<rect
								x={index}
								y={0}
								width={1}
								height={HEIGHT}
								{...stylex.props(styles.hitTarget)}
							>
								<title>
									{`${formatDateTime(bucket.start ?? "")}: ${
										missing ? "—" : format(value)
									} (${stateLabels[state]}${gapNote})`}
								</title>
							</rect>
						</g>
					);
				})}
				<line
					x1={0}
					x2={count}
					y1={HEIGHT}
					y2={HEIGHT}
					vectorEffect="non-scaling-stroke"
					{...stylex.props(styles.baseline)}
				/>
			</svg>
			<figcaption {...stylex.props(styles.footer)}>
				<span>{formatDateTime(usage.startTime ?? "")}</span>
				<span {...stylex.props(styles.legend)}>
					<LegendSwatch styles={styles.bar} label="Measured" />
					<LegendSwatch
						styles={[styles.bar, styles.incomplete]}
						label="Incomplete"
					/>
					<LegendSwatch styles={styles.missing} label="Missing" />
				</span>
				<span>{formatDateTime(usage.endTime ?? "")}</span>
			</figcaption>
		</figure>
	);
}

function LegendSwatch({
	styles: swatch,
	label,
}: {
	styles: stylex.StyleXStyles;
	label: string;
}) {
	return (
		<span {...stylex.props(styles.legendItem)}>
			<svg
				viewBox="0 0 10 10"
				aria-hidden="true"
				{...stylex.props(styles.swatch)}
			>
				<rect width={10} height={10} {...stylex.props(swatch)} />
			</svg>
			{label}
		</span>
	);
}

const styles = stylex.create({
	figure: {
		margin: "0rem",
		display: "flex",
		flexDirection: "column",
		gap: space.xs,
	},
	axisLabel: {
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.muted,
	},
	plot: { display: "block", width: "100%", height: "180px" },
	bar: { fill: colors.accent },
	incomplete: { fillOpacity: 0.35 },
	missing: { fill: colors.line, fillOpacity: 0.35 },
	hitTarget: {
		fill: { default: "transparent", ":hover": "rgba(240,232,220,0.06)" },
	},
	baseline: { stroke: colors.lineBright, strokeWidth: 1 },
	footer: {
		display: "flex",
		flexWrap: "wrap",
		justifyContent: "space-between",
		gap: space.md,
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.muted,
	},
	legend: { display: "flex", gap: space.md },
	legendItem: { display: "inline-flex", alignItems: "center", gap: "5px" },
	swatch: { width: "10px", height: "10px" },
});
