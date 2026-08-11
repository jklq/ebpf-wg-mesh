import { redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";

import type {
	DashboardDeploymentRecord,
	DashboardGitHubAccount,
	DashboardRepositoryInspection,
	DashboardServiceLogType,
	DashboardServicePosition,
	GitHubUserRepository,
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

export const fetchGitHubCatalog = createServerFn({ method: "GET" }).handler(
	async (): Promise<{
		githubAccount?: DashboardGitHubAccount;
		repositories: Array<GitHubUserRepository>;
	}> => {
		const svc = await import("#/lib/dashboard/server");
		return svc.loadGitHubCatalogFromSession();
	},
);

export const fetchRepositoryInspection = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => input as { repositorySelector: string })
	.handler(async ({ data }): Promise<DashboardRepositoryInspection | undefined> => {
		const svc = await import("#/lib/dashboard/server");
		return svc.inspectRepositorySourceFromSession(data);
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

export const fetchServiceDeployments = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				projectId: string;
				serviceId: string;
				limit?: number;
			},
	)
	.handler(async ({ data }): Promise<Array<DashboardDeploymentRecord>> => {
		const svc = await import("#/lib/dashboard/server");
		return svc.listServiceDeploymentsFromSession(data);
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

export const doRedeployService = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) => input as { projectId: string; serviceId: string },
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.redeployServiceFromSession(data);
	});

export const doDiscardServiceChanges = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				projectId: string;
				serviceId: string;
				changeIds?: Array<string>;
				discardAll?: boolean;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.discardServiceChangesFromSession(data);
	});

export const doSaveServicePosition = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				projectId: string;
				serviceId: string;
				position: DashboardServicePosition;
			},
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.saveServicePositionFromSession(data);
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

export const doRequestDomainOwnershipChallenge = createServerFn({
	method: "POST",
})
	.inputValidator(
		(input: unknown) => input as { projectId: string; hostname: string },
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.requestDomainOwnershipChallengeFromSession(data);
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
