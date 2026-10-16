import * as stylex from "@stylexjs/stylex";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, LayoutGrid, Pause, Play, Rows2 } from "lucide-react";
import { type ReactNode, useState } from "react";
import { Notice } from "#/components/ui/notice";
import { ServiceCostIndicator } from "#/features/billing/project-cost-panel";
import { UsageChartsSkeleton } from "#/features/dashboard/service-panel/panel-skeleton";
import {
	type ChartSeries,
	UsageLineChart,
} from "#/features/usage/usage-line-chart";
import {
	finiteDouble,
	formatAverageBytes,
	formatByteCount,
	formatVcpu,
	serviceUsageRanges,
	type UsageRange,
} from "#/features/usage/usage-model";
import { usagePanelLayout } from "#/features/usage/usage-panel-layout";
import {
	UsageRangePicker,
	usageControlStyles,
} from "#/features/usage/usage-range-picker";
import { UsageStaleNotice } from "#/features/usage/usage-status";
import { MEMORY_DEFINITION } from "#/features/usage/usage-summary";
import { useProjectUsage } from "#/features/usage/use-project-usage";
import { formatBytes } from "#/lib/bytes";
import type {
	DashboardProject,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { colors, fonts, shape } from "#/styles/tokens.stylex";

const cpuSeries: ChartSeries[] = [
	{
		label: "Usage",
		tone: "accent",
		value: (usage) => finiteDouble(usage.averageVcpu),
	},
];
const memorySeries: ChartSeries[] = [
	{
		label: "Usage",
		tone: "blue",
		value: (usage) => finiteDouble(usage.averageMemoryBytes),
	},
];
const networkSeries: ChartSeries[] = [
	{
		label: "Direct",
		tone: "accent",
		value: (usage) => Number(usage.publicDirectBytes),
	},
	{
		label: "Via ingress",
		tone: "blue",
		value: (usage) => Number(usage.publicIngressBytes),
	},
];

/** Charts of the service's measured usage over a chosen recent range. */
export function PanelUsage({
	service,
	project,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject;
}) {
	const [range, setRange] = useState<UsageRange>("1h");
	const [layout, setLayout] = useState<"grid" | "list">("grid");
	const [paused, setPaused] = useState(false);
	const [hovered, setHovered] = useState<number>();
	const { usage, error } = useProjectUsage({
		projectId: project.id,
		serviceId: service.id,
		range,
		paused,
	});
	const runtime = service.spec?.runtime;
	const cpuLimit = runtime?.cpuMillis
		? Number(runtime.cpuMillis) / 1000
		: undefined;
	const memoryLimit = runtime?.memoryMebibytes
		? Number(runtime.memoryMebibytes) * 1024 * 1024
		: undefined;

	return (
		<>
			<div {...stylex.props(usagePanelLayout.toolbar)}>
				<Link
					to="/projects/$projectId/usage"
					params={{ projectId: project.id }}
					{...stylex.props(styles.projectLink)}
				>
					Project usage <ArrowUpRight size={12} />
				</Link>
				<span {...stylex.props(styles.spacer)} />
				<fieldset {...stylex.props(styles.layoutToggle)}>
					<legend {...stylex.props(styles.hidden)}>Layout</legend>
					<LayoutButton
						label="List"
						selected={layout === "list"}
						onSelect={() => setLayout("list")}
					>
						<Rows2 size={14} />
					</LayoutButton>
					<LayoutButton
						label="Grid"
						selected={layout === "grid"}
						onSelect={() => setLayout("grid")}
					>
						<LayoutGrid size={14} />
					</LayoutButton>
				</fieldset>
				<UsageRangePicker
					ranges={serviceUsageRanges}
					selected={range}
					onSelect={setRange}
				/>
				<button
					type="button"
					aria-pressed={!paused}
					title={paused ? "Resume live updates" : "Pause live updates"}
					aria-label={paused ? "Resume live updates" : "Pause live updates"}
					onClick={() => setPaused((current) => !current)}
					{...stylex.props(usageControlStyles.control, !paused && styles.live)}
				>
					{paused ? <Play size={13} /> : <Pause size={13} />}
				</button>
			</div>

			<ServiceCostIndicator project={project} />
			{error && <Notice>Could not load usage: {error}</Notice>}
			{usage ? (
				<>
					<UsageStaleNotice usage={usage} />
					<div {...stylex.props(usagePanelLayout.cardsContainer)}>
						<div
							{...stylex.props(
								usagePanelLayout.cards,
								layout === "grid" && usagePanelLayout.grid,
							)}
						>
							<ChartCard
								title="CPU"
								meta={cpuLimit !== undefined && `Max ${formatVcpu(cpuLimit)}`}
							>
								<UsageLineChart
									usage={usage}
									series={cpuSeries}
									format={formatVcpu}
									kind="vcpu"
									limit={cpuLimit}
									label="Average CPU per bucket"
									hovered={hovered}
									onHover={setHovered}
								/>
							</ChartCard>
							<ChartCard
								title="Memory"
								meta={
									memoryLimit !== undefined &&
									`Max ${formatAverageBytes(memoryLimit)}`
								}
								hint={MEMORY_DEFINITION}
							>
								<UsageLineChart
									usage={usage}
									series={memorySeries}
									format={formatAverageBytes}
									kind="bytes"
									limit={memoryLimit}
									label="Average memory per bucket"
									hovered={hovered}
									onHover={setHovered}
								/>
							</ChartCard>
							<ChartCard
								title="Public network traffic"
								meta={
									usage.total &&
									`${formatByteCount(usage.total.publicEgressBytes)} total`
								}
								hint="Application bytes sent to the internet per bucket, before TLS."
								legend={networkSeries}
							>
								<UsageLineChart
									usage={usage}
									series={networkSeries}
									format={(bytes) => formatBytes(Math.round(bytes))}
									kind="bytes"
									label="Public output per bucket"
									hovered={hovered}
									onHover={setHovered}
								/>
							</ChartCard>
						</div>
					</div>
				</>
			) : (
				!error && <UsageChartsSkeleton />
			)}
		</>
	);
}

function LayoutButton({
	label,
	selected,
	onSelect,
	children,
}: {
	label: string;
	selected: boolean;
	onSelect: () => void;
	children: ReactNode;
}) {
	return (
		<button
			type="button"
			title={label}
			aria-label={label}
			aria-pressed={selected}
			onClick={onSelect}
			{...stylex.props(styles.layoutButton, selected && styles.selectedLayout)}
		>
			{children}
		</button>
	);
}

function ChartCard({
	title,
	meta,
	hint,
	legend,
	children,
}: {
	title: string;
	meta?: ReactNode;
	hint?: string;
	legend?: ChartSeries[];
	children: ReactNode;
}) {
	return (
		<section {...stylex.props(usagePanelLayout.card)}>
			<header {...stylex.props(usagePanelLayout.cardHeader)}>
				<h3 title={hint} {...stylex.props(styles.cardTitle)}>
					{title}
				</h3>
				{legend?.map((entry) => (
					<span key={entry.label} {...stylex.props(styles.legendItem)}>
						<span
							{...stylex.props(
								styles.swatch,
								entry.tone === "accent" ? styles.accent : styles.blue,
							)}
						/>
						{entry.label}
					</span>
				))}
				{meta && <span {...stylex.props(styles.meta)}>{meta}</span>}
			</header>
			{children}
		</section>
	);
}

const styles = stylex.create({
	spacer: { flex: "1" },
	projectLink: {
		display: "inline-flex",
		alignItems: "center",
		gap: "3px",
		fontSize: "12px",
		color: { default: colors.muted, ":hover": colors.ink },
		textDecorationLine: "none",
	},
	layoutToggle: {
		margin: "0rem",
		display: "inline-flex",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.card,
		padding: "0rem",
		overflow: "hidden",
	},
	hidden: {
		position: "absolute",
		width: "1px",
		height: "1px",
		overflow: "hidden",
		clipPath: "inset(50%)",
		whiteSpace: "nowrap",
	},
	layoutButton: {
		display: "inline-flex",
		width: "32px",
		height: "28px",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "none",
		backgroundColor: {
			default: colors.surfaceRaised,
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		color: colors.muted,
	},
	selectedLayout: { backgroundColor: colors.accentDim, color: colors.accent },
	live: { color: colors.healthy },
	cardTitle: {
		margin: "0rem",
		fontSize: "13px",
		fontWeight: "600",
		color: colors.ink,
	},
	meta: {
		marginLeft: "auto",
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.muted,
	},
	legendItem: {
		display: "inline-flex",
		alignItems: "center",
		gap: "5px",
		fontSize: "11px",
		color: colors.muted,
	},
	swatch: { width: "7px", height: "7px", borderRadius: "50%" },
	accent: { backgroundColor: colors.accent },
	blue: { backgroundColor: colors.unapplied },
});
