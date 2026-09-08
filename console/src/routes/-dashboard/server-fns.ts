import { redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { z } from "zod";

import type {
	DashboardDeploymentRecord,
	DashboardGitHubAccount,
	GitHubUserRepository,
} from "#/lib/dashboard/core/types.server";

const identifier = z.string().min(1);
const optionalIdentifier = identifier.optional();
const environmentIdInput = z.object({ environmentId: identifier });
const serviceIdInput = z.object({ serviceId: identifier });
const hostnameInput = z.object({ hostname: identifier });
const targetPort = z.union([z.string(), z.number()]).optional();
const deploymentAction = z.enum([
	"DEPLOYMENT_ACTION_RESTART",
	"DEPLOYMENT_ACTION_EXACT_REDEPLOY",
	"DEPLOYMENT_ACTION_ROLLBACK",
	"DEPLOYMENT_ACTION_CANCEL",
	"DEPLOYMENT_ACTION_REMOVE",
	"DEPLOYMENT_ACTION_RETRY",
]);
const serviceLogType = z.enum([
	"SERVICE_LOG_TYPE_RUNTIME",
	"SERVICE_LOG_TYPE_BUILD",
	"SERVICE_LOG_TYPE_DEPLOY",
	"SERVICE_LOG_TYPE_HTTP",
	"SERVICE_LOG_TYPE_NETWORK",
	"SERVICE_LOG_TYPE_UNSPECIFIED",
]);
const restartPolicy = z.enum([
	"RESTART_POLICY_ALWAYS",
	"RESTART_POLICY_ON_FAILURE",
	"RESTART_POLICY_NEVER",
]);
const restartSpec = z.object({
	policy: restartPolicy,
	maxRestarts: z.number().int().optional(),
	windowSeconds: z.number().int().optional(),
	initialDelayMs: z.number().int().optional(),
	maxDelayMs: z.number().int().optional(),
	backoffMultiplier: z.number().optional(),
	jitter: z.number().optional(),
	stableAfterSeconds: z.number().int().optional(),
});
const rollingStrategy = z.object({
	healthcheckTimeoutSeconds: z.number().int(),
	drainingSeconds: z.number().int(),
});
const updateServiceInput = z.object({
	serviceId: identifier,
	serviceName: z.string().optional(),
	runtimeEnv: z.record(z.string(), z.string()).optional(),
	cpuMillis: z.number().optional(),
	memoryMebibytes: z.number().optional(),
	repositorySelector: z.string().optional(),
	trackedRef: z.string().optional(),
	dockerfilePath: z.string().optional(),
	contextDir: z.string().optional(),
	restart: restartSpec.optional(),
	desiredReplicaCount: z.number().int().optional(),
	placementRegion: z.string().optional(),
	rollingStrategy: rollingStrategy.optional(),
});
const confirmRepositoryInput = z.object({
	repositorySelector: identifier,
	serviceName: z.string().optional(),
	trackedRef: z.string().optional(),
	dockerfilePath: z.string().optional(),
	contextDir: z.string().optional(),
	cpuMillis: z.number().optional(),
	memoryMebibytes: z.number().optional(),
});

export const loadHome = createServerFn({ method: "GET" })
	.inputValidator((input: unknown) =>
		z.object({ environmentId: optionalIdentifier }).optional().parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		const state = await svc.loadDashboardHome(data?.environmentId);
		if (!state) {
			throw redirect({
				to: "/login",
				search: { redirect: undefined, error: undefined },
			});
		}
		return state;
	});

export const doCreateEnvironment = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z.object({ projectId: identifier, name: identifier }).parse(input),
	)
	.handler(async ({ data }) =>
		(await import("#/lib/dashboard/server")).createEnvironmentFromSession(data),
	);

export const doDuplicateEnvironment = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({
				sourceEnvironmentId: identifier,
				name: identifier,
				copyVariables: z.boolean(),
			})
			.parse(input),
	)
	.handler(async ({ data }) =>
		(await import("#/lib/dashboard/server")).duplicateEnvironmentFromSession(
			data,
		),
	);

export const doRenameEnvironment = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z.object({ environmentId: identifier, name: identifier }).parse(input),
	)
	.handler(async ({ data }) =>
		(await import("#/lib/dashboard/server")).renameEnvironmentFromSession(data),
	);

export const doDeleteEnvironment = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => environmentIdInput.parse(input))
	.handler(async ({ data }) =>
		(await import("#/lib/dashboard/server")).deleteEnvironmentFromSession(
			data.environmentId,
		),
	);

export const doReleaseEnvironment = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => environmentIdInput.parse(input))
	.handler(async ({ data }) =>
		(await import("#/lib/dashboard/server")).releaseEnvironmentFromSession(
			data.environmentId,
		),
	);

export const fetchGitHubCatalog = createServerFn({ method: "GET" }).handler(
	async (): Promise<{
		githubAccount?: DashboardGitHubAccount;
		repositories: Array<GitHubUserRepository>;
	}> => {
		const svc = await import("#/lib/dashboard/server");
		return svc.loadGitHubCatalogFromSession();
	},
);

export const fetchServiceLogs = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({
				serviceId: identifier,
				allocationId: z.string().optional(),
				limit: z.number().int().optional(),
				logType: serviceLogType.optional(),
				buildId: z.string().optional(),
				search: z.string().optional(),
				startTime: z.date().optional(),
				endTime: z.date().optional(),
			})
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.listServiceLogsFromSession(data);
	});

export const fetchServiceDeployments = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({ serviceId: identifier, limit: z.number().int().optional() })
			.parse(input),
	)
	.handler(async ({ data }): Promise<Array<DashboardDeploymentRecord>> => {
		const svc = await import("#/lib/dashboard/server");
		return svc.listServiceDeploymentsFromSession(data);
	});

export const fetchDomainBindings = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => serviceIdInput.parse(input))
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.listDomainBindingsFromSession(data);
	});

export const doUpdateService = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => updateServiceInput.parse(input))
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.updateServiceFromSession(data);
	});

export const doApplyDeploymentAction = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({
				serviceId: identifier,
				deploymentId: identifier,
				action: deploymentAction,
				idempotencyKey: identifier,
				allocationId: z.string().optional(),
			})
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.applyDeploymentActionFromSession(data);
	});

export const doScaleService = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({ serviceId: identifier, desiredReplicaCount: z.number().int() })
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.scaleServiceFromSession(data);
	});

export const doDiscardServiceChanges = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({
				serviceId: identifier,
				changeIds: z.array(identifier).optional(),
				discardAll: z.boolean().optional(),
			})
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.discardServiceChangesFromSession(data);
	});

export const doDeleteService = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => serviceIdInput.parse(input))
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		await svc.deleteServiceFromSession(data);
	});

export const doSaveServicePosition = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({
				environmentId: identifier,
				serviceId: identifier,
				position: z.object({ x: z.number(), y: z.number() }),
			})
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.saveServicePositionFromSession(data);
	});

export const doCreateDomainBinding = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({ serviceId: identifier, hostname: identifier, targetPort })
			.transform((data) => ({ ...data, targetPort: data.targetPort }))
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.createDomainBindingFromSession(data);
	});

export const doGenerateDomainBinding = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({ serviceId: identifier, targetPort })
			.transform((data) => ({ ...data, targetPort: data.targetPort }))
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.generateDomainBindingFromSession(data);
	});

export const doUpdateDomainBinding = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({ serviceId: identifier, hostname: identifier, targetPort })
			.transform((data) => ({ ...data, targetPort: data.targetPort }))
			.parse(input),
	)
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.updateDomainBindingFromSession(data);
	});

export const doDeleteDomainBinding = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => hostnameInput.parse(input))
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.deleteDomainBindingFromSession(data);
	});

export const doCreateServiceFast = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => confirmRepositoryInput.parse(input))
	.handler(async ({ data }) => {
		const svc = await import("#/lib/dashboard/server");
		return svc.createServiceFastFromSession(data);
	});
