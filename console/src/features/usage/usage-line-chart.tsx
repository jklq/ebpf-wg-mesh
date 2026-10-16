import * as stylex from "@stylexjs/stylex";
import type { PointerEvent } from "react";
import {
	axisMax,
	axisTicks,
	type BucketState,
	bucketState,
	formatAxisTime,
	formatBucketTime,
	isMissing,
} from "#/features/usage/usage-model";
import { usagePanelLayout } from "#/features/usage/usage-panel-layout";
import type {
	DashboardProjectUsage,
	DashboardUsageQuantities,
} from "#/lib/dashboard/core/types.server";
import { dateMillis } from "#/lib/time";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

const HEIGHT = 100;
const TIME_LABELS = 3;

export type SeriesTone = "accent" | "blue";

export type ChartSeries = {
	label: string;
	tone: SeriesTone;
	value: (usage: DashboardUsageQuantities) => number | undefined;
};

const missingLabels: Partial<Record<BucketState, string>> = {
	pending: "Not measured yet",
	gap: "Not measured: gap",
	expired: "Past retention",
};

/** SVG path through the defined values; undefined values break the line. */
function linePath(values: (number | undefined)[], max: number): string {
	let path = "";
	let drawing = false;
	values.forEach((value, index) => {
		if (value === undefined) {
			drawing = false;
			return;
		}
		const y = HEIGHT - (Math.min(value, max) / max) * HEIGHT;
		path += `${drawing ? "L" : "M"}${index + 0.5} ${y} `;
		drawing = true;
	});
	return path.trim();
}

const percent = (value: number) => `${value}%`;

/**
 * A bucket series as lines in SVG user units: x is the bucket index, y the
 * share of the axis. Missing buckets break the line over a shaded band;
 * incomplete buckets are dashed. The hovered bucket is shared, so charts of
 * one range show their tooltips at the same time.
 */
export function UsageLineChart({
	usage,
	series,
	format,
	kind,
	limit,
	label,
	hovered,
	onHover,
}: {
	usage: DashboardProjectUsage;
	series: ChartSeries[];
	format: (value: number) => string;
	kind: "vcpu" | "bytes";
	limit?: number;
	label: string;
	hovered: number | undefined;
	onHover: (index: number | undefined) => void;
}) {
	const buckets = usage.buckets.map((bucket) => {
		const state = bucketState(bucket, usage);
		return {
			start: dateMillis(bucket.start),
			state,
			values: series.map((entry) =>
				isMissing(state) || !bucket.usage
					? undefined
					: entry.value(bucket.usage),
			),
		};
	});
	const count = Math.max(buckets.length, 1);
	const max = axisMax(
		Math.max(
			0,
			...buckets.flatMap((bucket) => bucket.values.map((v) => v ?? 0)),
		),
		limit,
		kind,
	);
	const spanMs = dateMillis(usage.endTime) - dateMillis(usage.startTime);
	const timeLabels = Array.from({ length: TIME_LABELS }, (_, index) =>
		Math.round(((index + 0.5) * count) / TIME_LABELS - 0.5),
	).filter((index) => buckets[index]);
	const active = hovered === undefined ? undefined : buckets[hovered];
	const left = (index: number) => ((index + 0.5) / count) * 100;

	const track = (event: PointerEvent<SVGSVGElement>) => {
		const box = event.currentTarget.getBoundingClientRect();
		const index = Math.floor(((event.clientX - box.left) / box.width) * count);
		onHover(Math.min(count - 1, Math.max(0, index)));
	};

	return (
		<figure {...stylex.props(usagePanelLayout.figure)}>
			<div {...stylex.props(usagePanelLayout.valueAxis)} aria-hidden="true">
				{axisTicks(max, kind)
					.reverse()
					.map((tick) => (
						<span key={tick} {...stylex.props(styles.valueLabel)}>
							{format(tick)}
						</span>
					))}
			</div>
			<div {...stylex.props(usagePanelLayout.plot)}>
				<svg
					viewBox={`0 0 ${count} ${HEIGHT}`}
					preserveAspectRatio="none"
					role="img"
					aria-label={label}
					onPointerMove={track}
					onPointerLeave={() => onHover(undefined)}
					{...stylex.props(styles.svg)}
				>
					{axisTicks(max, kind).map((tick) => (
						<line
							key={tick}
							x1={0}
							x2={count}
							y1={HEIGHT - (tick / max) * HEIGHT}
							y2={HEIGHT - (tick / max) * HEIGHT}
							vectorEffect="non-scaling-stroke"
							{...stylex.props(styles.grid)}
						/>
					))}
					{buckets.map(
						(bucket, index) =>
							isMissing(bucket.state) && (
								<rect
									key={bucket.start}
									x={index}
									y={0}
									width={1}
									height={HEIGHT}
									{...stylex.props(styles.missing)}
								/>
							),
					)}
					{series.map((entry, position) => {
						const values = buckets.map((bucket) => bucket.values[position]);
						const incomplete = (index: number) =>
							buckets[index]?.state === "incomplete";
						return (
							<g key={entry.label}>
								<path
									d={linePath(
										values.map((value, index) =>
											incomplete(index) ? undefined : value,
										),
										max,
									)}
									vectorEffect="non-scaling-stroke"
									{...stylex.props(styles.line, toneStroke[entry.tone])}
								/>
								<path
									d={linePath(
										values.map((value, index) =>
											incomplete(index) || incomplete(index + 1)
												? value
												: undefined,
										),
										max,
									)}
									vectorEffect="non-scaling-stroke"
									{...stylex.props(
										styles.line,
										styles.incomplete,
										toneStroke[entry.tone],
									)}
								/>
							</g>
						);
					})}
					{active && hovered !== undefined && (
						<line
							x1={hovered + 0.5}
							x2={hovered + 0.5}
							y1={0}
							y2={HEIGHT}
							vectorEffect="non-scaling-stroke"
							{...stylex.props(styles.crosshair)}
						/>
					)}
				</svg>
				{active &&
					hovered !== undefined &&
					active.values.map(
						(value, position) =>
							value !== undefined && (
								<span
									key={series[position]?.label}
									{...stylex.props(
										styles.dot,
										toneFill[series[position]?.tone ?? "accent"],
										styles.at(
											percent(left(hovered)),
											percent(100 - (Math.min(value, max) / max) * 100),
										),
									)}
								/>
							),
					)}
				{active && hovered !== undefined && (
					<div
						aria-hidden="true"
						{...stylex.props(
							styles.tooltip,
							left(hovered) > 55
								? styles.tooltipLeft(percent(100 - left(hovered)))
								: styles.tooltipRight(percent(left(hovered))),
						)}
					>
						<div {...stylex.props(styles.tooltipTime)}>
							{formatBucketTime(active.start)}
							{active.state === "incomplete" && " · incomplete"}
						</div>
						{series.map((entry, position) => {
							const value = active.values[position];
							return (
								<div key={entry.label} {...stylex.props(styles.tooltipRow)}>
									<span
										{...stylex.props(styles.swatch, toneFill[entry.tone])}
									/>
									<span {...stylex.props(styles.tooltipLabel)}>
										{entry.label}
									</span>
									<span {...stylex.props(styles.tooltipValue)}>
										{value === undefined
											? (missingLabels[active.state] ?? "No data")
											: format(value)}
									</span>
								</div>
							);
						})}
						{limit !== undefined && (
							<div {...stylex.props(styles.tooltipRow)}>
								<span {...stylex.props(styles.swatch, styles.limitSwatch)} />
								<span {...stylex.props(styles.tooltipLabel)}>Limit</span>
								<span {...stylex.props(styles.tooltipValue)}>
									{format(limit)}
								</span>
							</div>
						)}
					</div>
				)}
			</div>
			<div aria-hidden="true" />
			<div {...stylex.props(usagePanelLayout.timeAxis)} aria-hidden="true">
				{timeLabels.map((index) => (
					<span
						key={index}
						{...stylex.props(
							styles.timeLabel,
							styles.atX(percent(left(index))),
						)}
					>
						{formatAxisTime(buckets[index]?.start ?? 0, spanMs)}
					</span>
				))}
			</div>
		</figure>
	);
}

const toneStroke = stylex.create({
	accent: { stroke: colors.accent },
	blue: { stroke: colors.unapplied },
});

const toneFill = stylex.create({
	accent: { backgroundColor: colors.accent },
	blue: { backgroundColor: colors.unapplied },
});

const styles = stylex.create({
	valueLabel: {
		display: "flex",
		height: "0px",
		alignItems: "center",
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "10px",
		color: colors.dim,
	},
	svg: {
		display: "block",
		width: "100%",
		height: "100%",
		overflow: "visible",
		cursor: "crosshair",
	},
	grid: { stroke: colors.line, strokeOpacity: 0.45, strokeWidth: 1 },
	missing: { fill: colors.line, fillOpacity: 0.1 },
	line: {
		fill: "none",
		strokeWidth: 2,
		strokeLinecap: "round",
		strokeLinejoin: "round",
	},
	incomplete: { strokeDasharray: "3 4", strokeOpacity: 0.7 },
	crosshair: { stroke: colors.lineBright, strokeWidth: 1 },
	dot: {
		position: "absolute",
		width: "8px",
		height: "8px",
		borderRadius: "50%",
		outlineStyle: "solid",
		outlineWidth: "2px",
		outlineColor: colors.surface,
		transform: "translate(-50%, -50%)",
		pointerEvents: "none",
	},
	at: (left: string, top: string) => ({ left, top }),
	atX: (left: string) => ({ left }),
	tooltip: {
		position: "absolute",
		top: "0px",
		zIndex: 2,
		width: "max-content",
		minWidth: "11rem",
		display: "flex",
		flexDirection: "column",
		gap: "3px",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		borderRadius: shape.card,
		backgroundColor: colors.surfaceRaised,
		paddingInline: space.md,
		paddingBlock: space.sm,
		boxShadow: "0 12px 32px rgba(0,0,0,0.45)",
		pointerEvents: "none",
	},
	tooltipRight: (left: string) => ({ left, marginLeft: "12px" }),
	tooltipLeft: (right: string) => ({ right, marginRight: "12px" }),
	tooltipTime: {
		marginBottom: "2px",
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.muted,
	},
	tooltipRow: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		fontSize: "12px",
	},
	swatch: {
		width: "7px",
		height: "7px",
		flexShrink: "0",
		borderRadius: "50%",
	},
	limitSwatch: { backgroundColor: colors.dim },
	tooltipLabel: { flex: "1", whiteSpace: "nowrap", color: colors.label },
	tooltipValue: {
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "12px",
		color: colors.ink,
	},
	timeLabel: {
		position: "absolute",
		transform: "translateX(-50%)",
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "10px",
		color: colors.dim,
	},
});
