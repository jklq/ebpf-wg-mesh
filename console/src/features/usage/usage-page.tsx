import * as stylex from "@stylexjs/stylex";
import { ChartColumn, Download, Loader2 } from "lucide-react";
import { type ReactNode, useState } from "react";
import { PageShell } from "#/components/layout/page-shell";
import { Button } from "#/components/ui/button";
import { Notice } from "#/components/ui/notice";
import { textStyles } from "#/components/ui/text";
import { ProjectCostPanel } from "#/features/billing/project-cost-panel";
import { UsageChart } from "#/features/usage/usage-chart";
import {
	projectUsageRanges,
	type UsageMetric,
	type UsageRange,
	usageCsv,
	usageMetrics,
	usageRanges,
	usageWindow,
} from "#/features/usage/usage-model";
import { UsageResources } from "#/features/usage/usage-resources";
import { UsageFreshness, UsageGaps } from "#/features/usage/usage-status";
import { UsageSummary } from "#/features/usage/usage-summary";
import { useProjectUsage } from "#/features/usage/use-project-usage";
import type {
	DashboardProject,
	DashboardProjectUsage,
} from "#/lib/dashboard/core/types.server";
import { loadProjectUsage } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

export const INITIAL_USAGE_RANGE: UsageRange = "24h";

export function ProjectUsagePage({
	project,
	initial,
}: {
	project: DashboardProject;
	initial: { usage: DashboardProjectUsage; loadedAtMs: number };
}) {
	const [range, setRange] = useState<UsageRange>(INITIAL_USAGE_RANGE);
	const [metric, setMetric] = useState<UsageMetric>("cpu");
	const [exporting, setExporting] = useState(false);
	const [exportError, setExportError] = useState<string>();
	const { usage, loadedAtMs, loading, error } = useProjectUsage({
		projectId: project.id,
		range,
		initial,
	});

	const exportCsv = async () => {
		setExporting(true);
		setExportError(undefined);
		try {
			const full = await loadProjectUsage({
				data: {
					projectId: project.id,
					...usageWindow(range, Date.now()),
					resourceBuckets: true,
				},
			});
			download(`${project.name}-usage-${range}.csv`, usageCsv(full));
		} catch (cause) {
			setExportError(formatError(cause));
		} finally {
			setExporting(false);
		}
	};

	return (
		<PageShell
			title="Usage"
			icon={<ChartColumn size={15} />}
			back={{ href: `/projects/${project.id}`, label: project.name }}
			width="wide"
			actions={
				<>
					{loading && (
						<Loader2
							size={13}
							aria-label="Loading"
							{...stylex.props(styles.spinner)}
						/>
					)}
					<Segmented
						label="Time range"
						options={projectUsageRanges.map((id) => ({
							id,
							label: usageRanges[id].label,
						}))}
						selected={range}
						onSelect={setRange}
					/>
					<Button
						type="button"
						variant="ghost"
						disabled={exporting}
						onClick={() => void exportCsv()}
					>
						<Download size={13} /> Export CSV
					</Button>
				</>
			}
		>
			<div>
				<p {...stylex.props(textStyles.eyebrow)}>Project usage</p>
				<h1 {...stylex.props(styles.title)}>{project.name}</h1>
				<p {...stylex.props(styles.lede)}>
					Measured resource consumption of every replica and volume, in resource
					units.
				</p>
			</div>

			{error && <Notice>Could not refresh usage: {error}</Notice>}
			{exportError && <Notice>Could not export usage: {exportError}</Notice>}

			<ProjectCostPanel project={project} />

			{usage && (
				<>
					<UsageFreshness usage={usage} loadedAtMs={loadedAtMs} />
					<UsageSummary usage={usage} />

					<UsageSection
						title="Over time"
						actions={
							<Segmented
								label="Meter"
								options={Object.entries(usageMetrics).map(([id, entry]) => ({
									id: id as UsageMetric,
									label: entry.label,
								}))}
								selected={metric}
								onSelect={setMetric}
							/>
						}
					>
						<UsageChart usage={usage} metric={metric} />
					</UsageSection>

					<UsageSection
						title="Resources"
						lede="Averages cover the time each resource was measured. Deleted resources stay listed for the range they ran in."
					>
						<UsageResources resources={usage.resources} />
					</UsageSection>

					<UsageSection
						title="Gaps"
						lede="Time that was not measured. Gaps carry no usage and are not counted as zero."
					>
						<UsageGaps usage={usage} />
					</UsageSection>
				</>
			)}
		</PageShell>
	);
}

function download(fileName: string, text: string) {
	const url = URL.createObjectURL(new Blob([text], { type: "text/csv" }));
	const link = document.createElement("a");
	link.href = url;
	link.download = fileName;
	link.click();
	URL.revokeObjectURL(url);
}

function UsageSection({
	title,
	lede,
	actions,
	children,
}: {
	title: string;
	lede?: string;
	actions?: ReactNode;
	children: ReactNode;
}) {
	return (
		<section {...stylex.props(styles.section)}>
			<header {...stylex.props(styles.sectionHeader)}>
				<div>
					<h2 {...stylex.props(styles.sectionTitle)}>{title}</h2>
					{lede && <p {...stylex.props(styles.lede)}>{lede}</p>}
				</div>
				{actions}
			</header>
			{children}
		</section>
	);
}

function Segmented<Id extends string>({
	label,
	options,
	selected,
	onSelect,
}: {
	label: string;
	options: { id: Id; label: string }[];
	selected: Id;
	onSelect: (id: Id) => void;
}) {
	return (
		<fieldset {...stylex.props(styles.segmented)}>
			<legend {...stylex.props(styles.hidden)}>{label}</legend>
			{options.map((option) => (
				<button
					key={option.id}
					type="button"
					aria-pressed={option.id === selected}
					onClick={() => onSelect(option.id)}
					{...stylex.props(
						styles.segment,
						option.id === selected && styles.selectedSegment,
					)}
				>
					{option.label}
				</button>
			))}
		</fieldset>
	);
}

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

const styles = stylex.create({
	spinner: {
		color: colors.muted,
		animation: `${spin} 1s linear infinite`,
	},
	title: {
		marginTop: "0.125rem",
		marginBottom: "5px",
		fontFamily: fonts.display,
		fontSize: "32px",
		fontWeight: "500",
	},
	lede: {
		marginTop: space.xs,
		marginBottom: "0rem",
		fontSize: "13px",
		lineHeight: "1.45",
		color: colors.muted,
	},
	section: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		padding: "1.25rem",
	},
	sectionHeader: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "flex-start",
		justifyContent: "space-between",
		gap: space.md,
	},
	sectionTitle: {
		margin: "0rem",
		fontFamily: fonts.display,
		fontSize: "20px",
		fontWeight: "500",
		letterSpacing: "-0.025em",
		color: colors.ink,
	},
	segmented: {
		margin: "0rem",
		display: "inline-flex",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.control,
		padding: "0rem",
	},
	hidden: {
		position: "absolute",
		width: "1px",
		height: "1px",
		overflow: "hidden",
		clipPath: "inset(50%)",
		whiteSpace: "nowrap",
	},
	segment: {
		cursor: "pointer",
		borderStyle: "none",
		backgroundColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		paddingInline: "0.625rem",
		paddingBlock: "5px",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		letterSpacing: "0.07em",
		textTransform: "uppercase",
		color: colors.muted,
		transitionProperty: "color, background-color",
		transitionDuration: motion.fast,
	},
	selectedSegment: {
		backgroundColor: colors.accentDim,
		color: colors.accent,
	},
});
