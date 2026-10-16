// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	screen,
	waitFor,
	within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { summaryFixture } from "#/features/billing/billing.test-helpers";
import { BillingPage } from "#/features/billing/billing-page";
import { BillingRestrictionHint } from "#/features/billing/billing-status";
import { BUDGET_NOT_A_CEILING } from "#/features/billing/budget-editor";
import { checkoutNavigation } from "#/features/billing/plan-card";
import {
	StatementPage,
	statementDownload,
} from "#/features/billing/statements";
import type {
	DashboardBillingStatement,
	DashboardBillingSummary,
	DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import { jsonFixture } from "#/lib/dashboard/testkit/protocol";
import { renderWithRouter as render } from "#/lib/dashboard/testkit/render-with-router";
import {
	BillingStatementSchema,
	WorkspaceSchema,
} from "#/lib/platform-gen/platform_pb";

const mocks = vi.hoisted(() => ({
	doCancelBillingAccount: vi.fn(),
	doChangeBillingPlan: vi.fn(),
	doStartBillingActivation: vi.fn(),
	doStartBillingPaymentSetup: vi.fn(),
	doUpdateBillingBudget: vi.fn(),
	loadBillingStatement: vi.fn(),
	loadBillingSummary: vi.fn(),
}));
vi.mock("#/lib/dashboard/server-functions", () => mocks);

const workspace = jsonFixture(WorkspaceSchema, {
	id: "workspace-acme",
	name: "Acme",
	role: "WORKSPACE_ROLE_ADMIN",
});
const shell: DashboardWorkspaceShell = {
	user: { id: "user-1", email: "user@example.com" },
	workspace,
	workspaces: [workspace],
	invitations: [],
	canManageFleet: false,
};

function statementFixture(
	input: Parameters<typeof jsonFixture<typeof BillingStatementSchema>>[1],
): DashboardBillingStatement {
	return jsonFixture(BillingStatementSchema, {
		id: "statement-1",
		kind: "period_close",
		currency: "EUR",
		createdAt: "2026-10-02T00:00:00Z",
		...input,
	});
}

function renderBilling(
	summary: DashboardBillingSummary,
	options: {
		statements?: Array<DashboardBillingStatement>;
		liveProjectIds?: Array<string>;
	} = {},
) {
	return render(
		<BillingPage
			shell={shell}
			billing={{ available: true, summary }}
			statements={options.statements ?? []}
			liveProjectIds={options.liveProjectIds ?? []}
		/>,
	);
}

beforeEach(() => {
	for (const mock of Object.values(mocks)) mock.mockReset();
});
afterEach(() => cleanup());

describe("billing page", () => {
	it("shows a shadow preview restriction without implying enforcement", () => {
		renderBilling(
			summaryFixture({
				mode: "shadow",
				restriction: {
					restricted: true,
					dryRun: true,
					reason: "budget stop threshold reached",
				},
			}),
		);

		expect(screen.getByText("Shadow preview — would restrict")).toBeTruthy();
		expect(screen.getByText(/nothing is blocked or stopped/)).toBeTruthy();
		expect(screen.queryByRole("alert")).toBeNull();
	});

	it("explains an enforced restriction and routes payment failure to the hosted invoice", () => {
		renderBilling(
			summaryFixture({
				restriction: { growthBlocked: true, reason: "payment failed" },
			}),
			{
				statements: [
					statementFixture({
						state: "payment_failed",
						amountDueCents: "500",
						stripeHostedInvoiceUrl: "https://invoice.stripe.com/i/1",
					}),
				],
			},
		);

		const alert = screen.getByRole("alert");
		expect(alert.textContent).toMatch(/builds, environment releases/);
		expect(alert.textContent).toMatch(
			/Drafts and desired replica counts are kept/,
		);
		expect(
			within(alert)
				.getByRole("link", { name: /pay the outstanding invoice/i })
				.getAttribute("href"),
		).toBe("https://invoice.stripe.com/i/1");
	});

	it("breaks cost down by project, keeps deleted projects, and labels estimates", () => {
		renderBilling(
			summaryFixture({
				projectCosts: [
					{
						projectId: "web",
						projectName: "web",
						meter: "memory",
						quantity: "3600000",
						amountCents: "120",
					},
					{
						projectId: "gone",
						projectName: "old-api",
						meter: "cpu",
						quantity: "3600000000",
						amountCents: "200",
					},
				],
				gapMs: "5400000",
			}),
			{ liveProjectIds: ["web"] },
		);

		const deleted = screen.getByRole("region", { name: "old-api cost" });
		expect(within(deleted).getByText("Deleted")).toBeTruthy();
		const live = screen.getByRole("region", { name: "web cost" });
		expect(within(live).queryByText("Deleted")).toBeNull();
		expect(within(live).getByText(/Total cgroup memory/)).toBeTruthy();
		expect(screen.getByText(/assumes the last 24 hours/)).toBeTruthy();
		for (const coverage of screen.getAllByTestId("estimate-coverage"))
			expect(coverage.textContent).toMatch(/1\.5 h unmeasured/);
		expect(screen.getAllByTestId("estimate-coverage").length).toBeGreaterThan(
			2,
		);
	});

	it("saves a budget stop threshold in exact cents and says it is not a ceiling", async () => {
		const summary = summaryFixture();
		mocks.doUpdateBillingBudget.mockResolvedValue({
			...summary,
			budgetStopCents: "2550",
		});
		renderBilling(summary);

		expect(screen.getByText(new RegExp(BUDGET_NOT_A_CEILING))).toBeTruthy();
		fireEvent.change(screen.getByLabelText(/stop threshold/i), {
			target: { value: "25.5" },
		});
		fireEvent.click(screen.getByRole("button", { name: "Save threshold" }));

		await waitFor(() =>
			expect(mocks.doUpdateBillingBudget).toHaveBeenCalledWith({
				data: { workspaceId: "workspace-acme", budgetStopCents: "2550" },
			}),
		);
		expect(
			await screen.findByRole("meter", {
				name: "Gross usage against stop threshold",
			}),
		).toBeTruthy();
	});

	it("requires a retained-volume choice and shows storage charges before cancelling", async () => {
		mocks.doCancelBillingAccount.mockResolvedValue({
			effectiveAt: "2026-11-01T00:00:00Z",
		});
		renderBilling(
			summaryFixture({
				projectCosts: [
					{
						projectId: "web",
						projectName: "web",
						meter: "volume",
						quantity: "1",
						amountCents: "42",
					},
				],
			}),
		);

		fireEvent.click(screen.getByRole("button", { name: /cancel account/i }));
		const dialog = screen.getByRole("dialog", {
			name: "Cancel billing account",
		});
		expect(within(dialog).getByText("€0.15 per GiB-month")).toBeTruthy();
		expect(within(dialog).getByText("€0.42")).toBeTruthy();
		const confirm = within(dialog).getByRole("button", {
			name: "Cancel at period end",
		});
		expect((confirm as HTMLButtonElement).disabled).toBe(true);

		fireEvent.click(within(dialog).getByLabelText(/keep my volumes/i));
		fireEvent.click(confirm);

		await waitFor(() =>
			expect(mocks.doCancelBillingAccount).toHaveBeenCalledWith({
				data: { workspaceId: "workspace-acme" },
			}),
		);
		expect(
			await screen.findByText(/The account is cancelled effective/),
		).toBeTruthy();
	});

	it("activates a trial by issuing the first minimum and sending the admin to Checkout", async () => {
		const assign = vi
			.spyOn(checkoutNavigation, "assign")
			.mockImplementation(() => {});
		const trial = summaryFixture({ planId: "trial", state: "trial" });
		mocks.doStartBillingActivation.mockResolvedValue({
			...trial,
			state: "pending_activation",
			pendingPlanId: "hobby",
		});
		mocks.doStartBillingPaymentSetup.mockResolvedValue(
			"https://checkout.stripe.com/c/1",
		);
		renderBilling(trial);

		expect(screen.getAllByText(/does not start paid service/).length).toBe(2);
		fireEvent.click(screen.getByRole("button", { name: "Activate plan" }));

		await waitFor(() =>
			expect(assign).toHaveBeenCalledWith("https://checkout.stripe.com/c/1"),
		);
		expect(mocks.doStartBillingActivation).toHaveBeenCalledWith({
			data: { workspaceId: "workspace-acme", planId: "hobby" },
		});
		expect(mocks.doChangeBillingPlan).not.toHaveBeenCalled();
	});
});

describe("statement page", () => {
	it("explains lines, links the hosted invoice, and downloads the control plane's CSV", async () => {
		const save = vi
			.spyOn(statementDownload, "save")
			.mockImplementation(() => {});
		const statement = statementFixture({
			state: "open",
			amountDueCents: "500",
			stripeHostedInvoiceUrl: "https://invoice.stripe.com/i/2",
			lines: [
				{
					meter: "minimum",
					description: "Monthly minimum",
					amountCents: "500",
					periodStart: "2026-10-01T00:00:00Z",
					periodEnd: "2026-11-01T00:00:00Z",
				},
				{
					meter: "included_usage",
					description: "Included usage allowance",
					amountCents: "-320",
				},
			],
		});
		mocks.loadBillingStatement.mockResolvedValue({
			shell,
			statement: { ...statement, csv: "line,amount\n" },
		});
		render(<StatementPage shell={shell} statement={statement} />);

		expect(screen.getByText(/covered by the minimum/)).toBeTruthy();
		expect(screen.getByText("-€3.20")).toBeTruthy();
		expect(
			screen.getByRole("link", { name: /pay invoice/i }).getAttribute("href"),
		).toBe("https://invoice.stripe.com/i/2");

		fireEvent.click(screen.getByRole("button", { name: /download csv/i }));
		await waitFor(() =>
			expect(save).toHaveBeenCalledWith(
				"statement-statement-1.csv",
				"line,amount\n",
			),
		);
		expect(mocks.loadBillingStatement).toHaveBeenCalledWith({
			data: {
				workspaceId: "workspace-acme",
				statementId: "statement-1",
				includeCsv: true,
			},
		});
	});
});

describe("billing restriction hint", () => {
	it("explains only the control plane's billing refusal", () => {
		const { rerender } = render(
			<BillingRestrictionHint message="Deploy stopped: billing: account is restricted: budget stop threshold reached" />,
		);
		expect(screen.getByRole("link", { name: /open billing/i })).toBeTruthy();

		rerender(<BillingRestrictionHint message="Deploy stopped: timeout" />);
		expect(screen.queryByRole("link")).toBeNull();
	});
});
