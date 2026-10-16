import * as stylex from "@stylexjs/stylex";
import { CreditCard } from "lucide-react";
import { useId, useState } from "react";
import { Button } from "#/components/ui/button";
import { fieldStyles } from "#/components/ui/field";
import { Notice } from "#/components/ui/notice";
import {
	accountStateLabel,
	formatCents,
	planLabel,
	type SelectablePlan,
	selectablePlans,
} from "#/features/billing/billing-model";
import type { DashboardBillingSummary } from "#/lib/dashboard/core/types.server";
import {
	doChangeBillingPlan,
	doStartBillingActivation,
	doStartBillingPaymentSetup,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { formatDateTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

/** Sends the browser to Stripe's hosted Checkout; replaceable in tests. */
export const checkoutNavigation = {
	assign: (url: string) => window.location.assign(url),
};

export const SAVED_CARD_NOTE =
	"Saving a payment method does not start paid service. The plan activates once its first minimum invoice is paid.";

/** Plan display and change, paid activation, and the payment method entry. */
export function PlanCard({
	summary,
	canManage,
	onChanged,
}: {
	summary: DashboardBillingSummary;
	canManage: boolean;
	onChanged: (summary: DashboardBillingSummary) => void;
}) {
	const selectId = useId();
	const choices = selectablePlans.filter(
		(plan) => plan !== summary.planId && plan !== summary.pendingPlanId,
	);
	const [plan, setPlan] = useState<SelectablePlan | undefined>(choices[0]);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const enabled = summary.mode === "enabled";
	const activating =
		enabled &&
		(summary.state === "trial" || summary.state === "pending_activation");
	const cancelled = summary.state === "cancelled";
	const selected = plan && choices.includes(plan) ? plan : choices[0];

	const run = async (action: () => Promise<void>) => {
		setBusy(true);
		setError(undefined);
		try {
			await action();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};

	const setupPayment = async () => {
		const url = await doStartBillingPaymentSetup({
			data: { workspaceId: summary.workspaceId },
		});
		checkoutNavigation.assign(url);
	};

	const changePlan = () =>
		run(async () => {
			if (!selected) return;
			const data = { workspaceId: summary.workspaceId, planId: selected };
			if (!activating) {
				onChanged(await doChangeBillingPlan({ data }));
				return;
			}
			const next = await doStartBillingActivation({ data });
			onChanged(next);
			// Without a saved card, Checkout collects one and its webhook
			// continues the activation.
			if (!next.paymentReady) await setupPayment();
		});

	return (
		<div {...stylex.props(styles.card)}>
			<dl {...stylex.props(styles.facts)}>
				<Fact label="Plan" value={planLabel(summary.planId)} />
				<Fact label="Account" value={accountStateLabel(summary.state)} />
				<Fact
					label="Monthly minimum"
					value={formatCents(summary.monthlyMinimumCents, summary.currency)}
				/>
				<Fact
					label="Included usage"
					value={formatCents(summary.includedUsageCents, summary.currency)}
				/>
			</dl>
			<p {...stylex.props(styles.text)}>
				The minimum and included usage are fixed for the current period. Usage
				up to the included amount costs nothing beyond the minimum.
			</p>
			{summary.pendingPlanId && (
				<p {...stylex.props(styles.text, styles.pending)}>
					{summary.state === "pending_activation"
						? `${planLabel(summary.pendingPlanId)} activates when its first minimum invoice is paid.`
						: `Changes to ${planLabel(summary.pendingPlanId)} when the next period starts${summary.periodEnd ? ` on ${formatDateTime(summary.periodEnd)}` : ""}.`}
				</p>
			)}

			{canManage && !cancelled && choices.length > 0 && (
				<div {...stylex.props(styles.row)}>
					<label htmlFor={selectId}>
						<span {...stylex.props(fieldStyles.label)}>
							{activating ? "Upgrade to" : "Change plan to"}
						</span>
						<select
							id={selectId}
							value={selected}
							disabled={busy}
							onChange={(event) =>
								setPlan(event.target.value as SelectablePlan)
							}
							{...stylex.props(fieldStyles.input, styles.select)}
						>
							{choices.map((choice) => (
								<option key={choice} value={choice}>
									{planLabel(choice)}
								</option>
							))}
						</select>
					</label>
					<Button
						variant="primary"
						disabled={busy || !selected}
						onClick={() => void changePlan()}
					>
						{activating ? "Activate plan" : "Change next period"}
					</Button>
				</div>
			)}
			{canManage && !cancelled && (
				<p {...stylex.props(styles.text)}>
					{activating
						? `Activation invoices the plan's first minimum now. ${SAVED_CARD_NOTE}`
						: "Plan changes take effect when the next period starts; this period keeps its minimum and included usage."}
				</p>
			)}
			{cancelled && (
				<p {...stylex.props(styles.text)}>
					A cancelled account cannot change plan.
				</p>
			)}

			<div {...stylex.props(styles.payment)}>
				<CreditCard size={14} {...stylex.props(styles.icon)} />
				<div {...stylex.props(styles.paymentText)}>
					<strong {...stylex.props(styles.paymentTitle)}>Payment method</strong>
					<span {...stylex.props(styles.text)}>
						{!enabled
							? "Billing runs in shadow mode here: nothing is charged, so no payment method is needed."
							: summary.paymentReady
								? "A payment method is saved with Stripe."
								: `No payment method is saved. ${SAVED_CARD_NOTE}`}
					</span>
				</div>
				{enabled && canManage && !cancelled && (
					<Button disabled={busy} onClick={() => void run(setupPayment)}>
						{summary.paymentReady
							? "Replace payment method"
							: "Add payment method"}
					</Button>
				)}
			</div>
			{error && <Notice>{error}</Notice>}
		</div>
	);
}

function Fact({ label, value }: { label: string; value: string }) {
	return (
		<div {...stylex.props(styles.fact)}>
			<dt {...stylex.props(fieldStyles.label)}>{label}</dt>
			<dd {...stylex.props(styles.factValue)}>{value}</dd>
		</div>
	);
}

const styles = stylex.create({
	card: { display: "flex", flexDirection: "column", gap: space.md },
	facts: {
		margin: 0,
		display: "grid",
		gridTemplateColumns: "repeat(auto-fill, minmax(9rem, 1fr))",
		gap: space.md,
	},
	fact: { display: "flex", flexDirection: "column" },
	factValue: {
		margin: 0,
		fontFamily: fonts.mono,
		fontSize: "14px",
		color: colors.ink,
	},
	text: {
		margin: 0,
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	pending: { color: colors.unapplied },
	row: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "flex-end",
		gap: space.md,
	},
	select: { minWidth: "10rem" },
	payment: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: space.md,
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderTopColor: colors.line,
		paddingTop: space.md,
	},
	icon: { color: colors.muted },
	paymentText: {
		display: "flex",
		minWidth: 0,
		flex: "1 1 16rem",
		flexDirection: "column",
		gap: "2px",
	},
	paymentTitle: { fontSize: "13px", color: colors.ink },
});
