import * as stylex from "@stylexjs/stylex";
import { Link } from "@tanstack/react-router";
import { buttonStyles } from "#/components/ui/button";
import { accountStateLabel, planLabel } from "#/features/billing/billing-model";
import { SAVED_CARD_NOTE } from "#/features/billing/plan-card";
import { useBillingSummary } from "#/features/billing/use-billing-summary";
import { SettingsCard } from "#/features/project-settings/settings-card";
import { WorkspacePageHeader } from "#/features/workspace/page-header";
import type {
	DashboardBillingAvailability,
	DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import { colors, space } from "#/styles/tokens.stylex";

/** Where Stripe Checkout returns after saving (or abandoning) a payment method. */
export function CheckoutReturnPage({
	outcome,
	shell,
	billing: initial,
}: {
	outcome: "success" | "cancel";
	shell: DashboardWorkspaceShell;
	billing: DashboardBillingAvailability;
}) {
	// Stripe confirms the saved method by webhook, so keep the summary fresh.
	const { billing } = useBillingSummary({
		workspaceId: shell.workspace.id,
		initial,
	});
	const summary = billing?.available ? billing.summary : undefined;
	const success = outcome === "success";

	return (
		<>
			<WorkspacePageHeader
				title={success ? "Payment method saved" : "Payment setup cancelled"}
				lede={shell.workspace.name}
			/>
			<SettingsCard title={success ? "What happens next" : "Nothing changed"}>
				{success ? (
					<>
						<p {...stylex.props(styles.text)}>
							{summary?.paymentReady
								? "Stripe confirmed the payment method."
								: "Waiting for Stripe to confirm the payment method. This page updates on its own."}
						</p>
						<p {...stylex.props(styles.text, styles.important)}>
							{SAVED_CARD_NOTE}
						</p>
						{summary?.state === "pending_activation" && (
							<p {...stylex.props(styles.text)}>
								{`The first minimum invoice for the ${planLabel(summary.pendingPlanId)} plan is issued next; paid service starts once it is paid.`}
							</p>
						)}
					</>
				) : (
					<p {...stylex.props(styles.text)}>
						No payment method was saved and nothing was charged.
					</p>
				)}
				{summary && (
					<p {...stylex.props(styles.text)}>
						Account: {accountStateLabel(summary.state)} ·{" "}
						{planLabel(summary.planId)} plan
					</p>
				)}
				<Link
					to="/workspace/billing"
					search={{ workspace: shell.workspace.id }}
					{...stylex.props(
						buttonStyles.base,
						buttonStyles.primary,
						styles.back,
					)}
				>
					Back to billing
				</Link>
			</SettingsCard>
		</>
	);
}

const styles = stylex.create({
	text: {
		margin: 0,
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	important: { color: colors.ink },
	back: { alignSelf: "flex-start", marginTop: space.sm },
});
