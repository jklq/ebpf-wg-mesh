import { Code } from "@connectrpc/connect";
import { requireSession } from "#/lib/dashboard/core/auth.server";
import { loadWorkspaceShell } from "#/lib/dashboard/core/operations-workspaces.server";
import type { DashboardRuntime } from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardBillingAvailability,
	type DashboardBillingStatement,
	type DashboardBillingSummary,
	type DashboardUser,
	type DashboardWorkspaceShell,
	PlatformGatewayError,
} from "#/lib/dashboard/core/types.server";
import { PlatformService } from "#/lib/platform-gen/platform_pb";

const statementListLimit = 24;

export type WorkspaceBilling = {
	shell: DashboardWorkspaceShell;
	billing: DashboardBillingAvailability;
	statements: Array<DashboardBillingStatement>;
	/** Live project IDs, so the cost breakdown can mark deleted projects. */
	liveProjectIds: Array<string>;
};

/** The workspace billing page: account summary, recent statements, and live projects. */
export async function loadWorkspaceBilling(
	runtime: DashboardRuntime,
	requestedWorkspaceId?: string,
): Promise<WorkspaceBilling> {
	const shell = await loadWorkspaceShell(runtime, requestedWorkspaceId);
	const workspaceId = shell.workspace.id;
	const billing = await billingAvailability(runtime, shell.user, workspaceId);
	if (!billing.available)
		return { shell, billing, statements: [], liveProjectIds: [] };
	const [statements, projects] = await Promise.all([
		runtime.platform.call(
			PlatformService.method.listBillingStatements,
			shell.user,
			{ workspaceId, limit: statementListLimit },
		),
		runtime.platform.call(PlatformService.method.listProjects, shell.user, {
			workspaceId,
		}),
	]);
	return {
		shell,
		billing,
		statements: statements.statements,
		liveProjectIds: projects.projects.map((project) => project.id),
	};
}

export async function loadBillingSummaryFromSession(
	runtime: DashboardRuntime,
	workspaceId: string,
): Promise<DashboardBillingAvailability> {
	const session = await requireSession(runtime);
	return billingAvailability(runtime, session.user, workspaceId);
}

export async function loadBillingStatementFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId?: string; statementId: string; includeCsv?: boolean },
): Promise<{
	shell: DashboardWorkspaceShell;
	statement: DashboardBillingStatement;
}> {
	const shell = await loadWorkspaceShell(runtime, input.workspaceId);
	const statement = await runtime.platform.call(
		PlatformService.method.getBillingStatement,
		shell.user,
		{
			workspaceId: shell.workspace.id,
			statementId: input.statementId,
			includeCsv: input.includeCsv,
		},
	);
	return { shell, statement };
}

export async function updateBillingBudgetFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; budgetStopCents: string },
): Promise<DashboardBillingSummary> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.updateBillingBudget,
		session.user,
		input,
	);
}

export async function changeBillingPlanFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; planId: string },
): Promise<DashboardBillingSummary> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.changeBillingPlan,
		session.user,
		input,
	);
}

export async function startBillingActivationFromSession(
	runtime: DashboardRuntime,
	input: { workspaceId: string; planId: string },
): Promise<DashboardBillingSummary> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.startBillingActivation,
		session.user,
		input,
	);
}

/** Returns the Stripe-hosted Checkout URL that saves a payment method. */
export async function startBillingPaymentSetupFromSession(
	runtime: DashboardRuntime,
	workspaceId: string,
): Promise<string> {
	const session = await requireSession(runtime);
	const response = await runtime.platform.call(
		PlatformService.method.startBillingPaymentSetup,
		session.user,
		{ workspaceId },
	);
	if (!response.checkoutUrl.startsWith("https://"))
		throw new Error("The control plane returned no payment setup URL.");
	return response.checkoutUrl;
}

export async function cancelBillingAccountFromSession(
	runtime: DashboardRuntime,
	workspaceId: string,
): Promise<{ effectiveAt?: string }> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.cancelBillingAccount,
		session.user,
		{ workspaceId },
	);
}

async function billingAvailability(
	runtime: DashboardRuntime,
	user: DashboardUser,
	workspaceId: string,
): Promise<DashboardBillingAvailability> {
	try {
		const summary = await runtime.platform.call(
			PlatformService.method.getBillingAccount,
			user,
			{ workspaceId },
		);
		return { available: true, summary };
	} catch (error) {
		if (error instanceof PlatformGatewayError) {
			if (error.grpcCode === Code.FailedPrecondition)
				return {
					available: false,
					reason: "Billing is not enabled on this installation.",
				};
			if (error.grpcCode === Code.NotFound)
				return {
					available: false,
					reason:
						"This workspace has no billing account yet. Accounts are opened by the next billing pass, usually within a minute.",
				};
		}
		throw error;
	}
}
