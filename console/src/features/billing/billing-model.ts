import {
	formatByteCount,
	formatByteHours,
	formatCpuTime,
} from "#/features/usage/usage-model";
import type {
	DashboardBillingProjectCost,
	DashboardBillingRate,
	DashboardBillingStatement,
	DashboardBillingStatementLine,
	DashboardBillingSummary,
} from "#/lib/dashboard/core/types.server";
import { safeInteger } from "#/lib/platform-json";

/** Current-period estimates move with hourly rollups; a minute keeps them fresh. */
export const BILLING_REFRESH_MS = 60_000;

/** Plans a workspace admin can choose; trial and storage are assigned by billing. */
export const selectablePlans = ["hobby", "pro"] as const;
export type SelectablePlan = (typeof selectablePlans)[number];

const planLabels: Record<string, string> = {
	trial: "Trial",
	hobby: "Hobby",
	pro: "Pro",
	storage: "Storage only",
};

export function planLabel(planId: string): string {
	return planLabels[planId] ?? (planId || "None");
}

const accountStateLabels: Record<string, string> = {
	trial: "Trial",
	pending_activation: "Awaiting first payment",
	active: "Active",
	past_due: "Past due",
	cancelled: "Cancelled",
};

export function accountStateLabel(state: string): string {
	return accountStateLabels[state] ?? state;
}

const statementStates: Record<
	string,
	{ label: string; tone: "healthy" | "building" | "failed" | "offline" }
> = {
	shadow: { label: "Shadow preview", tone: "offline" },
	finalized: { label: "Finalized", tone: "building" },
	open: { label: "Awaiting payment", tone: "building" },
	paid: { label: "Paid", tone: "healthy" },
	payment_failed: { label: "Payment failed", tone: "failed" },
	accumulating: { label: "Below minimum charge", tone: "offline" },
	carried: { label: "Carried forward", tone: "offline" },
	waived: { label: "Waived", tone: "offline" },
	void: { label: "Void", tone: "offline" },
};

export function statementState(state: string) {
	return statementStates[state] ?? { label: state, tone: "offline" as const };
}

export function statementKindLabel(kind: string): string {
	return kind === "activation" ? "Activation" : "Period statement";
}

/** Formats integer cents the control plane returned; never derives an amount. */
export function formatCents(cents: string | number, currency = "EUR"): string {
	const value = typeof cents === "number" ? cents : safeInteger(cents);
	return new Intl.NumberFormat("en-IE", {
		style: "currency",
		currency: currency || "EUR",
	}).format(value / 100);
}

export function isZeroCents(cents: string): boolean {
	return cents === "0" || cents === "";
}

export type UsageMeter = "cpu" | "memory" | "volume" | "egress";

const meterLabels: Record<UsageMeter, string> = {
	cpu: "CPU",
	memory: "Memory",
	volume: "Volume storage",
	egress: "Public egress",
};

export function isUsageMeter(meter: string): meter is UsageMeter {
	return meter in meterLabels;
}

export function meterLabel(meter: string): string {
	return isUsageMeter(meter) ? meterLabels[meter] : meter;
}

/** Shows a stored-unit quantity (cpu-microseconds, byte-milliseconds, bytes) in readable units. */
export function formatMeterQuantity(meter: string, quantity: string): string {
	switch (meter) {
		case "cpu":
			return formatCpuTime(quantity);
		case "memory":
		case "volume":
			return formatByteHours(quantity);
		case "egress":
			return formatByteCount(quantity);
		default:
			return quantity;
	}
}

const GIB = 1024n ** 3n;
/** Billing converts monthly reference prices with a 30-day month. */
const MONTH_SECONDS = 2_592_000n;
const rateUnits: Record<UsageMeter, { perUnit: bigint; label: string }> = {
	cpu: { perUnit: 1_000_000n * MONTH_SECONDS, label: "vCPU-month" },
	memory: { perUnit: GIB * 1000n * MONTH_SECONDS, label: "GiB-month" },
	volume: { perUnit: GIB * 1000n * MONTH_SECONDS, label: "GiB-month" },
	egress: { perUnit: GIB, label: "GiB" },
};
const RATE_DECIMALS = 4;

/**
 * Re-expresses an exact per-stored-unit rate (cents per cpu-microsecond,
 * byte-millisecond, or byte) per readable unit, e.g. "€20.00 per vCPU-month".
 * This is a unit conversion of the returned rate with exact integers;
 * inexact results are marked "≈".
 */
export function formatRate(rate: DashboardBillingRate, currency = "EUR") {
	const unit = isUsageMeter(rate.meter) ? rateUnits[rate.meter] : undefined;
	if (
		!unit ||
		!/^\d+$/.test(rate.numerator) ||
		!/^[1-9]\d*$/.test(rate.denominator)
	)
		return `${rate.numerator}/${rate.denominator} cents per ${rate.unit}`;
	const scale = 10n ** BigInt(RATE_DECIMALS);
	const numerator = BigInt(rate.numerator) * unit.perUnit * scale;
	const denominator = BigInt(rate.denominator) * 100n;
	const scaled = numerator / denominator;
	const exact = numerator % denominator === 0n;
	const whole = scaled / scale;
	let fraction = (scaled % scale).toString().padStart(RATE_DECIMALS, "0");
	while (fraction.length > 2 && fraction.endsWith("0"))
		fraction = fraction.slice(0, -1);
	const symbol = currency === "EUR" ? "€" : `${currency} `;
	return `${exact ? "" : "≈ "}${symbol}${whole.toLocaleString("en-IE")}.${fraction} per ${unit.label}`;
}

export function rateForMeter(
	summary: Pick<DashboardBillingSummary, "rates">,
	meter: UsageMeter,
): DashboardBillingRate | undefined {
	return summary.rates.find((rate) => rate.meter === meter);
}

export type ProjectCostGroup = {
	projectId: string;
	projectName: string;
	/** Undefined when the caller does not know which projects are live. */
	deleted?: boolean;
	totalCents: string;
	costs: Array<DashboardBillingProjectCost>;
};

/**
 * Groups the summary's per project × meter lines by project. The control
 * plane already allocates cents so the lines sum to gross usage; adding a
 * project's lines only totals amounts it returned.
 */
export function groupProjectCosts(
	costs: Array<DashboardBillingProjectCost>,
	liveProjectIds?: ReadonlyArray<string>,
): Array<ProjectCostGroup> {
	const live = liveProjectIds ? new Set(liveProjectIds) : undefined;
	const groups = new Map<
		string,
		Omit<ProjectCostGroup, "totalCents"> & { total: bigint }
	>();
	for (const cost of costs) {
		const group = groups.get(cost.projectId) ?? {
			projectId: cost.projectId,
			projectName: cost.projectName || cost.projectId || "Unattributed",
			deleted: live ? !live.has(cost.projectId) : undefined,
			total: 0n,
			costs: [],
		};
		group.total += BigInt(cost.amountCents);
		group.costs.push(cost);
		groups.set(cost.projectId, group);
	}
	return [...groups.values()]
		.sort((a, b) =>
			a.total === b.total
				? a.projectName.localeCompare(b.projectName)
				: a.total > b.total
					? -1
					: 1,
		)
		.map(({ total, ...group }) => ({
			...group,
			totalCents: total.toString(),
		}));
}

export type RestrictionView =
	| { kind: "none" }
	| { kind: "shadow"; reason: string; since?: string }
	| { kind: "growthBlocked"; reason: string; since?: string }
	| { kind: "stopped"; reason: string; since?: string };

export function restrictionView(
	summary: Pick<DashboardBillingSummary, "restriction">,
): RestrictionView {
	const restriction = summary.restriction;
	if (!restriction || (!restriction.restricted && !restriction.growthBlocked))
		return { kind: "none" };
	const detail = { reason: restriction.reason, since: restriction.since };
	if (restriction.dryRun) return { kind: "shadow", ...detail };
	return restriction.restricted
		? { kind: "stopped", ...detail }
		: { kind: "growthBlocked", ...detail };
}

const restrictedErrorPattern = /billing: account is restricted(?::\s*(.*))?/;

/**
 * Recognises the control plane's FailedPrecondition for growth under an
 * enforced restriction. Returns its reason ("" when none was given).
 */
export function billingRestrictionReason(message: string): string | undefined {
	const match = restrictedErrorPattern.exec(message);
	return match ? (match[1] ?? "").trim() : undefined;
}

/** The statement whose hosted invoice settles a failed or outstanding payment. */
export function invoiceToPay(
	statements: Array<DashboardBillingStatement>,
): DashboardBillingStatement | undefined {
	const payable = statements.filter(
		(statement) => statement.stripeHostedInvoiceUrl,
	);
	return (
		payable.find((statement) => statement.state === "payment_failed") ??
		payable.find((statement) => statement.state === "open")
	);
}

const lineExplanations: Record<string, string> = {
	minimum:
		"The plan's monthly minimum for the dated period. It covers usage up to the included allowance.",
	included_usage:
		"Usage covered by the minimum, up to the plan's included allowance.",
	credit:
		"Credit applied after the allowance, earliest expiry first. Credit never reduces the minimum.",
	prepaid:
		"This period's minimum was already paid when the period started, so it is not charged again.",
	next_minimum: "The next period's minimum, billed in advance for its dates.",
	carried_balance:
		"Earlier amounts below Stripe's minimum charge, carried into this statement.",
};

/** One-line explanation of how a statement line came about. */
export function statementLineExplanation(
	line: DashboardBillingStatementLine,
	currency: string,
): string {
	if (isUsageMeter(line.meter)) {
		const rate = formatRate(
			{
				meter: line.meter,
				unit: line.unit,
				numerator: line.rateNumerator,
				denominator: line.rateDenominator,
			},
			currency,
		);
		return `${formatMeterQuantity(line.meter, line.quantity)} at ${rate}.`;
	}
	return lineExplanations[line.meter] ?? line.description;
}

/**
 * Converts a euro amount typed by a person into integer cents, exactly.
 * Returns undefined for anything that is not a plain non-negative amount.
 */
export function parseCentsInput(text: string): string | undefined {
	const match = /^(\d{1,9})(?:[.,](\d{1,2}))?$/.exec(text.trim());
	if (!match) return undefined;
	const cents =
		BigInt(match[1]) * 100n + BigInt((match[2] ?? "").padEnd(2, "0"));
	return cents.toString();
}

export function centsInput(cents: string): string {
	if (isZeroCents(cents)) return "";
	const value = BigInt(cents);
	const fraction = (value % 100n).toString().padStart(2, "0");
	return `${value / 100n}.${fraction}`;
}

/** Share of a limit used, clamped to 0–1, for progress bars. */
export function usedFraction(usedCents: string, limitCents: string): number {
	const limit = safeInteger(limitCents);
	if (limit <= 0) return 0;
	return Math.min(1, Math.max(0, safeInteger(usedCents) / limit));
}

/** Resource time without measurements, as a readable duration. */
export function formatResourceTime(milliseconds: string): string {
	const minutes = Math.round(safeInteger(milliseconds) / 60_000);
	if (minutes < 60) return `${minutes} min`;
	const hours = minutes / 60;
	return `${hours < 10 ? hours.toFixed(1) : Math.round(hours)} h`;
}
