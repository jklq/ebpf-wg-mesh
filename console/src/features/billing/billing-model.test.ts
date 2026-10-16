import { describe, expect, it } from "vitest";
import { rateVersion1 } from "#/features/billing/billing.test-helpers";
import {
	billingRestrictionReason,
	formatCents,
	formatRate,
	groupProjectCosts,
	invoiceToPay,
	parseCentsInput,
	restrictionView,
	statementLineExplanation,
} from "#/features/billing/billing-model";
import { jsonFixture } from "#/lib/dashboard/testkit/protocol";
import {
	BillingProjectCostSchema,
	BillingStatementLineSchema,
	BillingStatementSchema,
} from "#/lib/platform-gen/platform_pb";

describe("billing model", () => {
	it("re-expresses exact per-unit rates in readable units", () => {
		expect(rateVersion1.map((rate) => formatRate(rate))).toEqual([
			"€20.00 per vCPU-month",
			"€10.00 per GiB-month",
			"€0.15 per GiB-month",
			"€0.05 per GiB",
		]);
		expect(
			formatRate({
				meter: "cpu",
				unit: "cpu-microseconds",
				numerator: "1",
				denominator: "7000000000",
			}),
		).toBe("≈ €3.7028 per vCPU-month");
	});

	it("formats returned cents, including negative statement lines", () => {
		expect(formatCents("123456")).toBe("€1,234.56");
		expect(formatCents("-500")).toBe("-€5.00");
	});

	it("groups project costs by project and marks deleted projects", () => {
		const cost = (projectId: string, meter: string, amountCents: string) =>
			jsonFixture(BillingProjectCostSchema, {
				projectId,
				projectName: projectId === "gone" ? "old-api" : "web",
				meter,
				quantity: "1",
				amountCents,
			});
		const groups = groupProjectCosts(
			[
				cost("web", "cpu", "120"),
				cost("gone", "cpu", "300"),
				cost("web", "memory", "250"),
			],
			["web"],
		);
		expect(
			groups.map(({ projectName, deleted, totalCents, costs }) => [
				projectName,
				deleted,
				totalCents,
				costs.length,
			]),
		).toEqual([
			["web", false, "370", 2],
			["old-api", true, "300", 1],
		]);
	});

	it("distinguishes shadow previews from enforced restrictions", () => {
		const restriction = (fields: object) =>
			restrictionView({
				restriction: {
					restricted: false,
					growthBlocked: false,
					dryRun: false,
					reason: "budget stop threshold reached",
					...fields,
				},
			});
		expect(restriction({}).kind).toBe("none");
		expect(restriction({ restricted: true, dryRun: true }).kind).toBe("shadow");
		expect(restriction({ restricted: true }).kind).toBe("stopped");
		expect(restriction({ growthBlocked: true }).kind).toBe("growthBlocked");
	});

	it("recognises the control plane's billing restriction error", () => {
		expect(
			billingRestrictionReason(
				"Deploy stopped: billing: account is restricted: payment failed",
			),
		).toBe("payment failed");
		expect(billingRestrictionReason("billing: account is restricted")).toBe("");
		expect(billingRestrictionReason("permission denied")).toBeUndefined();
	});

	it("parses typed euro amounts into exact cents", () => {
		expect(parseCentsInput("25")).toBe("2500");
		expect(parseCentsInput("25.5")).toBe("2550");
		expect(parseCentsInput("0,07")).toBe("7");
		expect(parseCentsInput("-1")).toBeUndefined();
		expect(parseCentsInput("1.234")).toBeUndefined();
	});

	it("routes failed payments to their hosted invoice first", () => {
		const statement = (id: string, state: string, url = `https://pay/${id}`) =>
			jsonFixture(BillingStatementSchema, {
				id,
				state,
				stripeHostedInvoiceUrl: url,
			});
		expect(
			invoiceToPay([
				statement("paid", "paid"),
				statement("open", "open"),
				statement("failed", "payment_failed"),
			])?.id,
		).toBe("failed");
		expect(invoiceToPay([statement("paid", "paid")])).toBeUndefined();
	});

	it("explains usage lines by quantity and rate, and account lines by kind", () => {
		const cpuRate = rateVersion1[0];
		expect(
			statementLineExplanation(
				jsonFixture(BillingStatementLineSchema, {
					meter: "cpu",
					quantity: String(3_600_000_000),
					unit: cpuRate.unit,
					rateNumerator: cpuRate.numerator,
					rateDenominator: cpuRate.denominator,
				}),
				"EUR",
			),
		).toBe("1 CPU·h at €20.00 per vCPU-month.");
		expect(
			statementLineExplanation(
				jsonFixture(BillingStatementLineSchema, {
					meter: "credit",
					description: "Credit applied",
				}),
				"EUR",
			),
		).toMatch(/never reduces the minimum/);
	});
});
