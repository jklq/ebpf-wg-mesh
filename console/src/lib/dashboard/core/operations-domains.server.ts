import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	optionalPlatformResult,
	parseTargetPort,
} from "#/lib/dashboard/core/runtime.server";
import type { DashboardDomainBinding } from "#/lib/dashboard/core/types.server";
import { normalizeHostname } from "#/lib/dashboard/domain/dns.server";
import { PlatformService } from "#/lib/platform-gen/platform_pb";

export async function listDomainBindingsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<Array<DashboardDomainBinding>> {
	const session = await requireSession(runtime);
	return (
		(await optionalPlatformResult(
			runtime.platform
				.call(PlatformService.method.listDomainBindings, session.user, input)
				.then((response) => response.bindings ?? []),
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
	return runtime.platform.call(
		PlatformService.method.generateDomainBinding,
		session.user,
		{
			serviceId: input.serviceId,
			targetPort,
		},
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
	return runtime.platform.call(
		PlatformService.method.createDomainBinding,
		session.user,
		{
			binding: {
				serviceId: input.serviceId,
				hostname: normalizedHostname,
				targetPort,
			},
		},
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
	return runtime.platform.call(
		PlatformService.method.updateDomainBinding,
		session.user,
		{
			hostname: normalizedHostname,
			binding: { serviceId: input.serviceId, targetPort },
		},
	);
}

export async function deleteDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: { hostname: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await runtime.platform.call(
		PlatformService.method.deleteDomainBinding,
		session.user,
		{ hostname: input.hostname },
	);
}
