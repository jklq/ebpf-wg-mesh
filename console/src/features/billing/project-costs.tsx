import * as stylex from "@stylexjs/stylex";
import { Badge } from "#/components/ui/badge";
import {
	formatCents,
	formatMeterQuantity,
	formatRate,
	meterLabel,
	type ProjectCostGroup,
} from "#/features/billing/billing-model";
import { MEMORY_DEFINITION } from "#/features/usage/usage-summary";
import type {
	DashboardBillingProjectCost,
	DashboardBillingRate,
} from "#/lib/dashboard/core/types.server";
import { colors, fonts, space } from "#/styles/tokens.stylex";

/** Per project × meter costs, with deleted projects kept under their frozen names. */
export function ProjectCostBreakdown({
	groups,
	currency,
}: {
	groups: Array<ProjectCostGroup>;
	currency: string;
}) {
	if (groups.length === 0)
		return <p {...stylex.props(styles.empty)}>No usage this period so far.</p>;
	return (
		<div {...stylex.props(styles.groups)}>
			{groups.map((group) => (
				<section
					key={group.projectId}
					aria-label={`${group.projectName} cost`}
					{...stylex.props(styles.group)}
				>
					<header {...stylex.props(styles.groupHeader)}>
						<strong {...stylex.props(styles.projectName)}>
							{group.projectName}
						</strong>
						{group.deleted && <Badge tone="offline">Deleted</Badge>}
						<span {...stylex.props(styles.spacer)} />
						<span {...stylex.props(styles.amount)}>
							{formatCents(group.totalCents, currency)}
						</span>
					</header>
					<MeterCostRows costs={group.costs} currency={currency} />
				</section>
			))}
		</div>
	);
}

export function MeterCostRows({
	costs,
	currency,
}: {
	costs: Array<DashboardBillingProjectCost>;
	currency: string;
}) {
	return (
		<ul {...stylex.props(styles.rows)}>
			{costs.map((cost) => (
				<li key={cost.meter} {...stylex.props(styles.row)}>
					<span {...stylex.props(styles.meter)}>
						{meterLabel(cost.meter)}
						{cost.meter === "memory" && (
							<span {...stylex.props(styles.definition)}>
								{MEMORY_DEFINITION}
							</span>
						)}
					</span>
					<span {...stylex.props(styles.quantity)}>
						{formatMeterQuantity(cost.meter, cost.quantity)}
					</span>
					<span {...stylex.props(styles.amount)}>
						{formatCents(cost.amountCents, currency)}
					</span>
				</li>
			))}
		</ul>
	);
}

/** The exact rates the period is priced at, shown per readable unit. */
export function RateList({
	rates,
	rateVersion,
	currency,
}: {
	rates: Array<DashboardBillingRate>;
	rateVersion?: string;
	currency: string;
}) {
	return (
		<div {...stylex.props(styles.rates)}>
			<dl {...stylex.props(styles.rateList)}>
				{rates.map((rate) => (
					<div key={rate.meter} {...stylex.props(styles.rate)}>
						<dt {...stylex.props(styles.meter)}>{meterLabel(rate.meter)}</dt>
						<dd {...stylex.props(styles.rateValue)}>
							{formatRate(rate, currency)}
						</dd>
					</div>
				))}
			</dl>
			{rateVersion && (
				<p {...stylex.props(styles.empty)}>
					Rate version {rateVersion}, fixed for this period. A month is 30 days.
				</p>
			)}
		</div>
	);
}

const styles = stylex.create({
	empty: { margin: 0, fontSize: "13px", color: colors.muted },
	groups: { display: "flex", flexDirection: "column", gap: space.md },
	group: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
	},
	groupHeader: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		paddingInline: space.md,
		paddingBlock: space.sm,
		backgroundColor: colors.surfaceRaised,
	},
	projectName: { fontSize: "13px", color: colors.ink },
	spacer: { flex: 1 },
	rows: { margin: 0, padding: 0, listStyleType: "none" },
	row: {
		display: "grid",
		gridTemplateColumns: "minmax(10rem, 1fr) auto minmax(5rem, auto)",
		alignItems: "baseline",
		gap: space.md,
		paddingInline: space.md,
		paddingBlock: space.sm,
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderTopColor: colors.line,
		fontSize: "13px",
	},
	meter: { display: "flex", flexDirection: "column", color: colors.label },
	definition: { fontSize: "11px", color: colors.dim },
	quantity: { fontFamily: fonts.mono, fontSize: "12px", color: colors.muted },
	amount: {
		fontFamily: fonts.mono,
		fontSize: "13px",
		color: colors.ink,
		textAlign: "right",
	},
	rates: { display: "flex", flexDirection: "column", gap: space.sm },
	rateList: {
		margin: 0,
		display: "grid",
		gridTemplateColumns: "repeat(auto-fill, minmax(12rem, 1fr))",
		gap: space.sm,
	},
	rate: { display: "flex", flexDirection: "column", gap: "2px" },
	rateValue: {
		margin: 0,
		fontFamily: fonts.mono,
		fontSize: "12px",
		color: colors.ink,
	},
});
