import * as stylex from "@stylexjs/stylex";
import { useId, useState } from "react";
import { Button } from "#/components/ui/button";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { Notice } from "#/components/ui/notice";
import {
	formatCents,
	formatRate,
	groupProjectCosts,
	rateForMeter,
} from "#/features/billing/billing-model";
import { EstimateCoverage } from "#/features/billing/billing-status";
import type { DashboardBillingSummary } from "#/lib/dashboard/core/types.server";
import { doCancelBillingAccount } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { formatDateTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

type VolumeChoice = "keep" | "delete";

/**
 * Cancels at period end. Volumes that still exist afterwards keep billing on
 * storage-only periods, so the person must say what happens to them, and
 * sees those continuing charges, before confirming.
 */
export function CancelBillingDialog({
	summary,
	onClose,
	onCancelled,
}: {
	summary: DashboardBillingSummary;
	onClose: () => void;
	onCancelled: (effectiveAt?: string) => void;
}) {
	const name = useId();
	const [choice, setChoice] = useState<VolumeChoice>();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const effective = summary.periodEnd
		? formatDateTime(summary.periodEnd)
		: "the end of the current period";
	const volumeRate = rateForMeter(summary, "volume");
	const volumeCosts = groupProjectCosts(
		summary.projectCosts.filter((cost) => cost.meter === "volume"),
	);

	const confirm = async () => {
		setBusy(true);
		setError(undefined);
		try {
			const result = await doCancelBillingAccount({
				data: { workspaceId: summary.workspaceId },
			});
			onCancelled(result.effectiveAt);
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<Dialog label="Cancel billing account" onClose={busy ? undefined : onClose}>
			<div {...stylex.props(dialogStyles.card, styles.card)}>
				<h2 {...stylex.props(styles.title)}>Cancel billing account</h2>
				<p {...stylex.props(styles.text)}>
					Cancellation takes effect at {effective}. This period is still billed,
					no new minimum starts, and services stop when it takes effect.
				</p>

				<section
					aria-label="Continuing storage charges"
					{...stylex.props(styles.storage)}
				>
					<strong {...stylex.props(styles.subtitle)}>
						Storage-only charges continue for volumes you keep
					</strong>
					<p {...stylex.props(styles.text)}>
						Volumes that still exist after {effective} are billed on
						storage-only periods — volume storage only, with no minimum — until
						you delete them.
					</p>
					<dl {...stylex.props(styles.facts)}>
						<dt>Volume storage rate</dt>
						<dd {...stylex.props(styles.value)}>
							{volumeRate
								? formatRate(volumeRate, summary.currency)
								: "Not available"}
						</dd>
						{volumeCosts.map((group) => (
							<div key={group.projectId} {...stylex.props(styles.pair)}>
								<dt>{group.projectName} volumes this period</dt>
								<dd {...stylex.props(styles.value)}>
									{formatCents(group.totalCents, summary.currency)}
								</dd>
							</div>
						))}
					</dl>
					{volumeCosts.length === 0 && (
						<p {...stylex.props(styles.text)}>
							No volume storage has been billed this period.
						</p>
					)}
					<EstimateCoverage summary={summary} />
				</section>

				<fieldset {...stylex.props(styles.choices)}>
					<legend {...stylex.props(styles.subtitle)}>
						What happens to your volumes?
					</legend>
					<label {...stylex.props(styles.choice)}>
						<input
							type="radio"
							name={name}
							checked={choice === "keep"}
							onChange={() => setChoice("keep")}
						/>
						<span>
							<strong>Keep my volumes.</strong> Their storage keeps billing at
							the rate above until I delete them.
						</span>
					</label>
					<label {...stylex.props(styles.choice)}>
						<input
							type="radio"
							name={name}
							checked={choice === "delete"}
							onChange={() => setChoice("delete")}
						/>
						<span>
							<strong>I will delete my volumes</strong> before {effective}. Any
							volume still present then keeps billing as storage.
						</span>
					</label>
				</fieldset>

				{error && <Notice>{error}</Notice>}
				<div {...stylex.props(styles.actions)}>
					<Button variant="ghost" disabled={busy} onClick={onClose}>
						Keep account
					</Button>
					<Button
						variant="dangerSolid"
						disabled={busy || choice === undefined}
						onClick={() => void confirm()}
					>
						{busy ? "Cancelling…" : "Cancel at period end"}
					</Button>
				</div>
			</div>
		</Dialog>
	);
}

const styles = stylex.create({
	card: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		padding: space.xl,
	},
	title: {
		margin: 0,
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
		color: colors.failed,
	},
	subtitle: {
		display: "block",
		marginBottom: space.xs,
		padding: 0,
		fontSize: "13px",
		fontWeight: "600",
		color: colors.ink,
	},
	text: {
		margin: 0,
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	storage: {
		display: "flex",
		flexDirection: "column",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		padding: space.md,
	},
	facts: {
		margin: 0,
		display: "grid",
		gridTemplateColumns: "1fr auto",
		gap: space.xs,
		fontSize: "13px",
		color: colors.label,
	},
	pair: { display: "contents" },
	value: {
		margin: 0,
		fontFamily: fonts.mono,
		fontSize: "12px",
		color: colors.ink,
		textAlign: "right",
	},
	choices: {
		margin: 0,
		display: "flex",
		flexDirection: "column",
		gap: space.sm,
		borderStyle: "none",
		padding: 0,
	},
	choice: {
		display: "flex",
		alignItems: "flex-start",
		gap: space.sm,
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	actions: { display: "flex", justifyContent: "flex-end", gap: space.sm },
});
