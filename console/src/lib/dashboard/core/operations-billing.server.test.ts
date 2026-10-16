import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import * as auth from "#/lib/dashboard/core/auth.server";
import * as billing from "#/lib/dashboard/core/operations-billing.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";
import { jsonFixture } from "#/lib/dashboard/testkit/protocol";
import {
	BillingAccountSummarySchema,
	BillingStatementSchema,
	ProjectSchema,
} from "#/lib/platform-gen/platform_pb";

async function signedInHarness() {
	const harness = createDashboardTestHarness();
	await auth.completeAuthCallback(harness.runtime, {
		userId: "user-1",
		email: "user@example.com",
		redirectTo: "/",
	});
	return harness;
}

describe("billing operations", () => {
	it("loads the summary, statements, and live projects for the billing page", async () => {
		const harness = await signedInHarness();
		harness.platform.billingSummary = jsonFixture(BillingAccountSummarySchema, {
			workspaceId: "workspace-personal",
			grossCents: "320",
		});
		harness.platform.billingStatements = [
			jsonFixture(BillingStatementSchema, { id: "statement-1" }),
		];
		harness.platform.projects = [
			jsonFixture(ProjectSchema, {
				id: "web",
				workspaceId: "workspace-personal",
			}),
			jsonFixture(ProjectSchema, {
				id: "gone",
				workspaceId: "workspace-personal",
				deletion: { deletedAt: "2026-10-01T00:00:00Z" },
			}),
		];

		const page = await billing.loadWorkspaceBilling(harness.runtime);

		expect(page.billing).toMatchObject({
			available: true,
			summary: { grossCents: "320" },
		});
		expect(page.statements.map((statement) => statement.id)).toEqual([
			"statement-1",
		]);
		expect(page.liveProjectIds).toEqual(["web"]);
	});

	it("reports billing that is off or not provisioned instead of failing the page", async () => {
		const harness = await signedInHarness();
		harness.platform.errors.getBillingAccount = new ConnectError(
			"billing is not configured",
			Code.FailedPrecondition,
		);
		await expect(
			billing.loadBillingSummaryFromSession(
				harness.runtime,
				"workspace-personal",
			),
		).resolves.toEqual({
			available: false,
			reason: "Billing is not enabled on this installation.",
		});

		harness.platform.errors.getBillingAccount = undefined;
		await expect(
			billing.loadBillingSummaryFromSession(
				harness.runtime,
				"workspace-personal",
			),
		).resolves.toMatchObject({ available: false });

		harness.platform.errors.getBillingAccount = new ConnectError(
			"billing: workspace access denied",
			Code.PermissionDenied,
		);
		await expect(
			billing.loadBillingSummaryFromSession(
				harness.runtime,
				"workspace-personal",
			),
		).rejects.toMatchObject({ grpcCode: Code.PermissionDenied });
	});
});
