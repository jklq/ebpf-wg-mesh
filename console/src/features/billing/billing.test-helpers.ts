import { jsonFixture } from "#/lib/dashboard/testkit/protocol";
import type { BillingAccountSummaryJson } from "#/lib/platform-gen/platform_pb";
import { BillingAccountSummarySchema } from "#/lib/platform-gen/platform_pb";

const GIB = 2n ** 30n;
const MONTH_MS = 2_592_000_000n;

/** Rate version 1 exactly as the control plane derives it, in cents per stored unit. */
export const rateVersion1 = [
	{
		meter: "cpu",
		unit: "cpu-microseconds",
		numerator: "1",
		denominator: "1296000000",
	},
	{
		meter: "memory",
		unit: "byte-milliseconds",
		numerator: "1000",
		denominator: String(GIB * MONTH_MS),
	},
	{
		meter: "volume",
		unit: "byte-milliseconds",
		numerator: "15",
		denominator: String(GIB * MONTH_MS),
	},
	{ meter: "egress", unit: "bytes", numerator: "5", denominator: String(GIB) },
];

export function summaryFixture(input: BillingAccountSummaryJson = {}) {
	return jsonFixture(BillingAccountSummarySchema, {
		accountId: "account-1",
		workspaceId: "workspace-acme",
		currency: "EUR",
		planId: "hobby",
		state: "active",
		mode: "enabled",
		periodStart: "2026-10-01T00:00:00Z",
		periodEnd: "2026-11-01T00:00:00Z",
		monthlyMinimumCents: "500",
		includedUsageCents: "500",
		grossCents: "320",
		estimatedPeriodTotalCents: "500",
		forecastGrossCents: "910",
		measuredMs: "3600000",
		watermark: "2026-10-16T14:00:00Z",
		restriction: {},
		rates: rateVersion1,
		rateVersion: "1",
		...input,
	});
}
