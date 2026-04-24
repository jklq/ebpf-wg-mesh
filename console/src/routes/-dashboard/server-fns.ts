import { redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";

import type {
	DashboardServiceLogType,
	UpdateServiceInput,
} from "#/lib/dashboard/core/types.server";

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

export const fetchServiceLogs = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				projectId: string;
				serviceId: string;
				allocationId?: string;
				limit?: number;
				logType?: DashboardServiceLogType;
				buildId?: string;
				search?: string;
				startTime?: Date;
				endTime?: Date;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.listServiceLogsFromSession(data);
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
			input as {
				projectId: string;
				serviceId: string;
				hostname: string;
				targetPort: string | number | undefined;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.createDomainBindingFromSession(data);
	});

export const doUpdateDomainBinding = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				projectId: string;
				serviceId: string;
				hostname: string;
				targetPort: string | number | undefined;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.updateDomainBindingFromSession(data);
	});

export const doDeleteDomainBinding = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) => input as { projectId: string; hostname: string },
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.deleteDomainBindingFromSession(data);
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
				serviceName?: string;
				trackedRef?: string;
				dockerfilePath?: string;
				contextDir?: string;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		await svc.confirmRepositoryFromSession(data);
		const state = await svc.loadDashboardHome();
		if (!state) {
			throw redirect({
				to: "/login",
				search: { redirect: undefined, error: undefined },
			});
		}
		return state;
	});

export const doCreateServiceFast = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				repositorySelector: string;
				serviceName?: string;
				trackedRef?: string;
				dockerfilePath?: string;
				contextDir?: string;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.createServiceFastFromSession(data);
	});
