import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	parseTargetPort,
	platformCall,
	safePlatformCall,
	safeVerifyHostname,
} from "#/lib/dashboard/core/runtime.server";
import type { DashboardDomainBinding } from "#/lib/dashboard/core/types.server";
import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";
import { normalizeHostname } from "#/lib/dashboard/domain/dns.server";

export async function listDomainBindingsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<Array<DashboardDomainBinding>> {
	const session = await requireSession(runtime);
	return (
		(await safePlatformCall(runtime, "listDomainBindings", (platform) =>
			platform.listDomainBindings(session.user, input),
		)) ?? []
	);
}

export async function generateDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		targetPort: string | number | undefined;
	},
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const targetPort = parseTargetPort(input.targetPort);
	return platformCall(runtime, "generateDomainBinding", (platform) =>
		platform.generateDomainBinding(session.user, {
			serviceId: input.serviceId,
			targetPort,
		}),
	);
}

export async function createDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		hostname: string;
		targetPort: string | number | undefined;
	},
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const normalizedHostname = normalizeHostname(input.hostname);
	const targetPort = parseTargetPort(input.targetPort);
	return platformCall(runtime, "createDomainBinding", (platform) =>
		platform.createDomainBinding(session.user, {
			serviceId: input.serviceId,
			hostname: normalizedHostname,
			targetPort,
		}),
	);
}

export async function updateDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		hostname: string;
		targetPort: string | number | undefined;
	},
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const normalizedHostname = normalizeHostname(input.hostname);
	const targetPort = parseTargetPort(input.targetPort);
	return platformCall(runtime, "updateDomainBinding", (platform) =>
		platform.updateDomainBinding(session.user, {
			hostname: normalizedHostname,
			serviceId: input.serviceId,
			targetPort,
		}),
	);
}

export async function deleteDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: { hostname: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await platformCall(runtime, "deleteDomainBinding", (platform) =>
		platform.deleteDomainBinding(session.user, { hostname: input.hostname }),
	);
}

export async function checkDomainDNSFromSession(
	runtime: DashboardRuntime,
	hostname: string,
): Promise<DomainVerificationResult | undefined> {
	await requireSession(runtime);
	return safeVerifyHostname(runtime, normalizeHostname(hostname));
}
