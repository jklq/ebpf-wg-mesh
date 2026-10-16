import * as stylex from "@stylexjs/stylex";
import { XCircle } from "lucide-react";
import { type ReactNode, useState } from "react";
import { Badge } from "#/components/ui/badge";
import { Button } from "#/components/ui/button";
import { Notice } from "#/components/ui/notice";
import {
	accountStateLabel,
	formatCents,
	groupProjectCosts,
	invoiceToPay,
	isZeroCents,
	planLabel,
} from "#/features/billing/billing-model";
import {
	EstimateCoverage,
	RestrictionNotice,
} from "#/features/billing/billing-status";
import { BudgetEditor } from "#/features/billing/budget-editor";
import { CancelBillingDialog } from "#/features/billing/cancel-billing-dialog";
import { PlanCard } from "#/features/billing/plan-card";
import { ProgressBar } from "#/features/billing/progress-bar";
import {
	ProjectCostBreakdown,
	RateList,
} from "#/features/billing/project-costs";
import { StatementList } from "#/features/billing/statements";
import { useBillingSummary } from "#/features/billing/use-billing-summary";
import { SettingsCard } from "#/features/project-settings/settings-card";
import { WorkspacePageHeader } from "#/features/workspace/page-header";
import { canManageWorkspace } from "#/features/workspace/roles";
import type {
	DashboardBillingAvailability,
	DashboardBillingStatement,
	DashboardBillingSummary,
	DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import { formatDateTime, formatLogTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function BillingPage({
	shell,
	billing: initial,
	statements,
	liveProjectIds,
}: {
	shell: DashboardWorkspaceShell;
	billing: DashboardBillingAvailability;
	statements: Array<DashboardBillingStatement>;
	liveProjectIds: Array<string>;
}) {
	const { billing, loadedAtMs, error, replace } = useBillingSummary({
		workspaceId: shell.workspace.id,
		initial,
	});
	const header = (
		<WorkspacePageHeader
			title="Billing"
			lede={`Usage cost, plan, and statements for ${shell.workspace.name}`}
		/>
	);
	if (!billing?.available)
		return (
			<>
				{header}
				<SettingsCard title="Billing unavailable">
					<p {...stylex.props(styles.text)}>
						{billing?.reason ?? "Billing could not be loaded."}
					</p>
				</SettingsCard>
			</>
		);
	return (
		<>
			{header}
			<BillingAccount
				shell={shell}
				summary={billing.summary}
				statements={statements}
				liveProjectIds={liveProjectIds}
				loadedAtMs={loadedAtMs}
				refreshError={error}
				onSummary={(summary) => replace({ available: true, summary })}
			/>
		</>
	);
}

function BillingAccount({
	shell,
	summary,
	statements,
	liveProjectIds,
	loadedAtMs,
	refreshError,
	onSummary,
}: {
	shell: DashboardWorkspaceShell;
	summary: DashboardBillingSummary;
	statements: Array<DashboardBillingStatement>;
	liveProjectIds: Array<string>;
	loadedAtMs?: number;
	refreshError?: string;
	onSummary: (summary: DashboardBillingSummary) => void;
}) {
	const canManage = canManageWorkspace(shell.workspace);
	const [cancelling, setCancelling] = useState(false);
	const { currency } = summary;
	const money = (cents: string) => formatCents(cents, currency);
	const shadow = summary.mode === "shadow";
	const trialEnds =
		summary.state === "trial" && summary.trialEndsAt
			? formatDateTime(summary.trialEndsAt)
			: undefined;

	return (
		<>
			<div {...stylex.props(styles.status)}>
				<Badge tone={shadow ? "offline" : "healthy"}>
					{shadow ? "Shadow mode" : "Live billing"}
				</Badge>
				<span {...stylex.props(styles.text)}>
					{shadow
						? "Statements are previews and restrictions are dry runs. Nothing is charged."
						: `${planLabel(summary.planId)} plan · ${accountStateLabel(summary.state)}`}
				</span>
				{loadedAtMs !== undefined && (
					<span {...stylex.props(styles.refreshed)}>
						Refreshed {formatLogTime(new Date(loadedAtMs))}
					</span>
				)}
			</div>
			{refreshError && (
				<Notice>Could not refresh billing: {refreshError}</Notice>
			)}
			<RestrictionNotice summary={summary} invoice={invoiceToPay(statements)} />
			{!isZeroCents(summary.unpaidCents) && (
				<Notice>
					{money(summary.unpaidCents)} is unpaid on finalized statements.
				</Notice>
			)}

			<SettingsCard
				title="Current period"
				lede={
					summary.periodStart
						? `${formatDateTime(summary.periodStart)} – ${formatDateTime(summary.periodEnd ?? "")}`
						: undefined
				}
			>
				<div {...stylex.props(styles.figures)}>
					<Figure
						label="Usage so far"
						value={money(summary.grossCents)}
						detail="Gross usage before the allowance and credits."
					/>
					<Figure
						label="Estimated period total"
						value={money(summary.estimatedPeriodTotalCents)}
						detail={`${money(summary.monthlyMinimumCents)} minimum plus ${money(summary.afterAllowanceCents)} usage after the allowance, less credit.`}
					/>
				</div>
				{!isZeroCents(summary.includedUsageCents) && (
					<ProgressBar
						label="Included usage"
						fraction={{
							used: summary.grossCents,
							limit: summary.includedUsageCents,
						}}
						caption={`${money(summary.grossCents)} of ${money(summary.includedUsageCents)} included usage`}
					/>
				)}
				<EstimateCoverage summary={summary} />
			</SettingsCard>

			<SettingsCard title="Forecast">
				<Figure
					label="Estimated gross usage at period end"
					value={money(summary.forecastGrossCents)}
					detail="An estimate that assumes the last 24 hours of hourly usage continue for the rest of the period. It is never billed and never triggers a restriction."
				/>
				<EstimateCoverage summary={summary} />
			</SettingsCard>

			<SettingsCard title="Credit">
				<Figure
					label="Credit balance"
					value={money(summary.creditBalanceCents)}
					detail="Applies to usage after the allowance, never to the minimum. The grant expiring first is used first."
				/>
				{trialEnds && (
					<p {...stylex.props(styles.text)}>
						The trial ends {trialEnds}. Unused trial credit expires then, and
						the workspace is restricted unless a paid plan is active.
					</p>
				)}
			</SettingsCard>

			<SettingsCard
				title="Cost by project"
				lede="This period's usage per project and meter. Deleted projects stay listed under their names."
			>
				<ProjectCostBreakdown
					groups={groupProjectCosts(summary.projectCosts, liveProjectIds)}
					currency={currency}
				/>
				<EstimateCoverage summary={summary} />
				<RateList
					rates={summary.rates}
					rateVersion={summary.rateVersion}
					currency={currency}
				/>
			</SettingsCard>

			<SettingsCard title="Plan">
				<PlanCard
					summary={summary}
					canManage={canManage}
					onChanged={onSummary}
				/>
			</SettingsCard>

			<SettingsCard title="Budget">
				<BudgetEditor
					summary={summary}
					canManage={canManage}
					onSaved={onSummary}
				/>
			</SettingsCard>

			<SettingsCard title="Statements">
				<StatementList
					workspaceId={shell.workspace.id}
					statements={statements}
				/>
			</SettingsCard>

			{summary.state === "cancelled" ? (
				<SettingsCard title="Cancellation" tone="danger">
					<p {...stylex.props(styles.text)}>
						{`The account is cancelled${summary.cancelEffectiveAt ? ` effective ${formatDateTime(summary.cancelEffectiveAt)}` : ""}. Volumes that still exist afterwards keep billing as storage only until you delete them.`}
					</p>
				</SettingsCard>
			) : (
				canManage && (
					<SettingsCard title="Danger zone" tone="danger">
						<Row
							title="Cancel billing account"
							description="Stops paid service at the end of this period. Kept volumes continue to bill for storage."
							action={
								<Button
									variant="dangerOutline"
									onClick={() => setCancelling(true)}
								>
									<XCircle size={13} /> Cancel account
								</Button>
							}
						/>
					</SettingsCard>
				)
			)}

			{cancelling && (
				<CancelBillingDialog
					summary={summary}
					onClose={() => setCancelling(false)}
					onCancelled={(effectiveAt) => {
						setCancelling(false);
						onSummary({
							...summary,
							state: "cancelled",
							pendingPlanId: "",
							cancelEffectiveAt: effectiveAt,
						});
					}}
				/>
			)}
		</>
	);
}

function Figure({
	label,
	value,
	detail,
}: {
	label: string;
	value: string;
	detail: string;
}) {
	return (
		<div {...stylex.props(styles.figure)}>
			<span {...stylex.props(styles.figureLabel)}>{label}</span>
			<span {...stylex.props(styles.figureValue)}>{value}</span>
			<span {...stylex.props(styles.text)}>{detail}</span>
		</div>
	);
}

function Row({
	title,
	description,
	action,
}: {
	title: string;
	description: string;
	action: ReactNode;
}) {
	return (
		<div {...stylex.props(styles.row)}>
			<div>
				<strong {...stylex.props(styles.rowTitle)}>{title}</strong>
				<span {...stylex.props(styles.text)}>{description}</span>
			</div>
			{action}
		</div>
	);
}

const styles = stylex.create({
	status: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: space.sm,
	},
	refreshed: {
		marginLeft: "auto",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.dim,
	},
	text: {
		display: "block",
		margin: 0,
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	figures: {
		display: "grid",
		gridTemplateColumns: "repeat(auto-fit, minmax(14rem, 1fr))",
		gap: space.lg,
	},
	figure: { display: "flex", flexDirection: "column", gap: space.xs },
	figureLabel: {
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.label,
	},
	figureValue: {
		fontFamily: fonts.display,
		fontSize: "30px",
		color: colors.ink,
	},
	row: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.lg,
	},
	rowTitle: {
		display: "block",
		marginBottom: "3px",
		fontSize: "13px",
		fontWeight: "600",
		color: colors.ink,
	},
});
