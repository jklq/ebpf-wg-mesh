import * as stylex from "@stylexjs/stylex";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, Euro } from "lucide-react";
import {
	formatCents,
	formatRate,
	groupProjectCosts,
	rateForMeter,
} from "#/features/billing/billing-model";
import {
	EstimateCoverage,
	RestrictionNotice,
} from "#/features/billing/billing-status";
import { MeterCostRows, RateList } from "#/features/billing/project-costs";
import { useBillingSummary } from "#/features/billing/use-billing-summary";
import type {
	DashboardBillingSummary,
	DashboardProject,
} from "#/lib/dashboard/core/types.server";
import { colors, fonts, space } from "#/styles/tokens.stylex";

function projectCost(summary: DashboardBillingSummary, projectId: string) {
	return groupProjectCosts(
		summary.projectCosts.filter((cost) => cost.projectId === projectId),
	)[0];
}

/** This project's share of its workspace's current-period cost. */
export function ProjectCostPanel({ project }: { project: DashboardProject }) {
	const { billing } = useBillingSummary({
		workspaceId: project.workspaceId,
		enabled: project.workspaceId !== "",
	});
	if (!billing?.available) return null;
	const { summary } = billing;
	const cost = projectCost(summary, project.id);
	return (
		<section aria-label="Project cost" {...stylex.props(styles.panel)}>
			<header {...stylex.props(styles.header)}>
				<div>
					<h2 {...stylex.props(styles.title)}>Cost this period</h2>
					<p {...stylex.props(styles.text)}>
						This project's usage priced at the workspace's rates. The monthly
						minimum, included allowance, and credits apply to the whole
						workspace.
					</p>
				</div>
				<span {...stylex.props(styles.total)}>
					{formatCents(cost?.totalCents ?? "0", summary.currency)}
				</span>
			</header>
			<RestrictionNotice summary={summary} compact />
			{cost ? (
				<MeterCostRows costs={cost.costs} currency={summary.currency} />
			) : (
				<p {...stylex.props(styles.text)}>No usage this period so far.</p>
			)}
			<EstimateCoverage summary={summary} />
			<RateList rates={summary.rates} currency={summary.currency} />
			<Link
				to="/workspace/billing"
				search={{ workspace: project.workspaceId }}
				{...stylex.props(styles.link)}
			>
				Workspace billing <ArrowUpRight size={12} />
			</Link>
		</section>
	);
}

/**
 * Compact cost context for a service: its project's period cost and the
 * CPU and memory rates its replicas are billed at.
 */
export function ServiceCostIndicator({
	project,
}: {
	project: DashboardProject;
}) {
	const { billing } = useBillingSummary({
		workspaceId: project.workspaceId,
		enabled: project.workspaceId !== "",
	});
	if (!billing?.available) return null;
	const { summary } = billing;
	const cost = projectCost(summary, project.id);
	const rates = (["cpu", "memory"] as const)
		.map((meter) => rateForMeter(summary, meter))
		.flatMap((rate) =>
			rate
				? [
						`${rate.meter === "cpu" ? "CPU" : "Memory"} ${formatRate(rate, summary.currency)}`,
					]
				: [],
		);
	return (
		<span
			data-testid="service-cost"
			title={`${rates.join(" · ")}. Memory is total cgroup memory, including page cache and kernel memory.`}
			{...stylex.props(styles.indicator)}
		>
			<Euro size={12} />
			Project {formatCents(cost?.totalCents ?? "0", summary.currency)} this
			period
			<EstimateCoverage summary={summary} />
		</span>
	);
}

const styles = stylex.create({
	panel: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		padding: "1.25rem",
	},
	header: {
		display: "flex",
		alignItems: "flex-start",
		justifyContent: "space-between",
		gap: space.md,
	},
	title: {
		margin: 0,
		fontFamily: fonts.display,
		fontSize: "20px",
		fontWeight: "500",
		letterSpacing: "-0.025em",
		color: colors.ink,
	},
	text: {
		marginTop: space.xs,
		marginBottom: 0,
		fontSize: "13px",
		lineHeight: "1.45",
		color: colors.muted,
	},
	total: { fontFamily: fonts.display, fontSize: "28px", color: colors.ink },
	link: {
		display: "inline-flex",
		alignItems: "center",
		gap: "2px",
		fontSize: "12px",
		color: colors.accent,
		textDecoration: "none",
	},
	indicator: {
		display: "inline-flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: space.xs,
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.muted,
	},
});
