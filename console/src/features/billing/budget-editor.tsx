import * as stylex from "@stylexjs/stylex";
import { type FormEvent, useId, useState } from "react";
import { Button } from "#/components/ui/button";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { Notice } from "#/components/ui/notice";
import {
	centsInput,
	formatCents,
	isZeroCents,
	parseCentsInput,
} from "#/features/billing/billing-model";
import { ProgressBar } from "#/features/billing/progress-bar";
import type { DashboardBillingSummary } from "#/lib/dashboard/core/types.server";
import { doUpdateBillingBudget } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, space } from "#/styles/tokens.stylex";

export const BUDGET_NOT_A_CEILING =
	"This is a stop threshold, not an absolute invoice ceiling: the monthly minimum and retained volume storage still bill after it is reached.";

/** Gross-usage stop threshold. Restricts the workspace once usage reaches it. */
export function BudgetEditor({
	summary,
	canManage,
	onSaved,
}: {
	summary: DashboardBillingSummary;
	canManage: boolean;
	onSaved: (summary: DashboardBillingSummary) => void;
}) {
	const inputId = useId();
	const [value, setValue] = useState(centsInput(summary.budgetStopCents));
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const hasBudget = !isZeroCents(summary.budgetStopCents);
	const cents = value.trim() === "" ? "0" : parseCentsInput(value);
	const unchanged = cents === (summary.budgetStopCents || "0");

	const save = async (budgetStopCents: string) => {
		setSaving(true);
		setError(undefined);
		try {
			const next = await doUpdateBillingBudget({
				data: { workspaceId: summary.workspaceId, budgetStopCents },
			});
			setValue(centsInput(next.budgetStopCents));
			onSaved(next);
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setSaving(false);
		}
	};

	const submit = (event: FormEvent) => {
		event.preventDefault();
		if (cents !== undefined) void save(cents);
	};

	return (
		<div {...stylex.props(styles.editor)}>
			<p {...stylex.props(styles.text)}>
				When gross usage this period (before the included allowance and credits)
				reaches the threshold, the workspace is restricted: services stop and
				new usage is blocked until the next period or a higher threshold.{" "}
				{BUDGET_NOT_A_CEILING}
			</p>
			{hasBudget && (
				<ProgressBar
					label="Gross usage against stop threshold"
					fraction={{
						used: summary.grossCents,
						limit: summary.budgetStopCents,
					}}
					caption={`${formatCents(summary.grossCents, summary.currency)} of ${formatCents(summary.budgetStopCents, summary.currency)}`}
				/>
			)}
			{canManage ? (
				<form onSubmit={submit} {...stylex.props(styles.form)}>
					<label htmlFor={inputId} {...stylex.props(styles.field)}>
						<span {...stylex.props(fieldStyles.label)}>
							Stop threshold ({summary.currency || "EUR"})
						</span>
						<TextInput
							id={inputId}
							inputMode="decimal"
							placeholder="No threshold"
							value={value}
							disabled={saving}
							aria-invalid={cents === undefined}
							onChange={(event) => setValue(event.target.value)}
						/>
					</label>
					<Button
						type="submit"
						variant="primary"
						disabled={saving || cents === undefined || unchanged}
					>
						{saving ? "Saving…" : "Save threshold"}
					</Button>
					{hasBudget && (
						<Button
							variant="ghost"
							disabled={saving}
							onClick={() => void save("0")}
						>
							Remove
						</Button>
					)}
				</form>
			) : (
				<p {...stylex.props(styles.text)}>
					{hasBudget
						? `Stop threshold: ${formatCents(summary.budgetStopCents, summary.currency)}.`
						: "No stop threshold is set."}{" "}
					Only workspace admins can change it.
				</p>
			)}
			{cents === undefined && (
				<Notice>Enter an amount such as 25 or 25.50.</Notice>
			)}
			{error && <Notice>{error}</Notice>}
		</div>
	);
}

const styles = stylex.create({
	editor: { display: "flex", flexDirection: "column", gap: space.md },
	text: {
		margin: 0,
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	form: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "flex-end",
		gap: space.md,
	},
	field: { flex: "0 1 14rem" },
});
