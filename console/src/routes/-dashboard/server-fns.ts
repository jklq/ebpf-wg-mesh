import { redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";

import type { UpdateServiceInput } from "#/lib/dashboard/core/types.server";

export const loadHome = createServerFn({ method: "GET" }).handler(async () => {
	const svc = await import("#/lib/dashboard/server");
	const state = await svc.loadDashboardHome();
	if (!state) {
		throw redirect({
			to: "/login",
			search: { redirect: undefined, error: undefined },
		});
	}
	return state;
});

export const fetchServiceStatus = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) => input as { projectId: string; serviceId: string },
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.getServiceStatusFromSession(data);
	});

export const fetchDomainBindings = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) => input as { projectId: string; serviceId: string },
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.listDomainBindingsFromSession(data);
	});

export const doUpdateService = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => input as UpdateServiceInput)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.updateServiceFromSession(data);
	});

export const doCreateDomainBinding = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as { projectId: string; serviceId: string; hostname: string },
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.createDomainBindingFromSession(data);
	});

export const doCheckDNS = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => input as { hostname: string })
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.checkDomainDNSFromSession(data.hostname);
	});

export const doInspectRepository = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => input as { repositorySelector: string })
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		await svc.inspectRepositoryFromSession(data);
		const state = await svc.loadDashboardHome();
		if (!state) {
			throw redirect({
				to: "/login",
				search: { redirect: undefined, error: undefined },
			});
		}
		return state;
	});

export const doConfirmRepository = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				repositorySelector: string;
				trackedRef?: string;
				dockerfilePath?: string;
				contextDir?: string;
				containerPort?: string;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.confirmRepositoryFromSession(data);
	});
