import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	platformCall,
	storeCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardDeploymentAction,
	type DashboardDeploymentRecord,
	type DashboardServiceLogLine,
	type DashboardServiceLogType,
	type DashboardServicePosition,
	type DashboardServiceRecord,
	type DashboardServiceStatus,
	type DashboardSourceSpec,
	DashboardValidationError,
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
	type UpdateServiceInput,
} from "#/lib/dashboard/core/types.server";
import { normalizeRepositorySelector } from "#/lib/dashboard/onboarding/flow";
import {
	applyServicePositions,
	normalizeResource,
	normalizeRuntimeEnv,
	normalizeServicePosition,
	requireGitHubRepositoryAccess,
} from "./operations-helpers.server";

export async function waitForServiceStatusFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	return platformCall(runtime, "waitForServiceStatus", (platform) =>
		platform.waitForServiceStatus(session.user, input),
	);
}

export async function listEnvironmentServicesFromSession(
	runtime: DashboardRuntime,
	input: { environmentId: string },
): Promise<Array<DashboardServiceRecord>> {
	const session = await requireSession(runtime);
	const [services, positions] = await Promise.all([
		platformCall(runtime, "listServices", (platform) =>
			platform.listServices(session.user, input.environmentId),
		),
		storeCall(runtime, "listServicePositions", (store) =>
			store.listServicePositions(session.user.id, input.environmentId),
		),
	]);
	return applyServicePositions(services, positions);
}

export async function waitForEnvironmentServicesFromSession(
	runtime: DashboardRuntime,
	input: {
		environmentId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	const result = await platformCall(runtime, "waitForServices", (platform) =>
		platform.waitForServices(session.user, input),
	);
	if (result.notModified || !result.services) {
		return result;
	}
	const positions = await storeCall(runtime, "listServicePositions", (store) =>
		store.listServicePositions(session.user.id, input.environmentId),
	);
	return {
		...result,
		services: applyServicePositions(result.services, positions),
	};
}

export async function waitForProjectServicesFromSession(
	runtime: DashboardRuntime,
	input: {
		projectId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	const environments = await platformCall(
		runtime,
		"listEnvironments",
		(platform) => platform.listEnvironments(session.user, input.projectId),
	);
	const environment =
		environments.find((entry) => entry.isProduction) ?? environments[0];
	if (!environment) {
		throw new DashboardValidationError({
			message: "project has no environment",
		});
	}
	const result = await platformCall(runtime, "waitForServices", (platform) =>
		platform.waitForServices(session.user, {
			environmentId: environment.id,
			waitIndex: input.waitIndex,
			waitTimeoutSeconds: input.waitTimeoutSeconds,
		}),
	);
	if (result.notModified || !result.services) {
		return result;
	}
	const positions = await storeCall(runtime, "listServicePositions", (store) =>
		store.listServicePositions(session.user.id, environment.id),
	);
	return {
		...result,
		services: applyServicePositions(result.services, positions),
	};
}

export async function listServiceLogsFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		allocationId?: string;
		limit?: number;
		logType?: DashboardServiceLogType;
		buildId?: string;
		search?: string;
		startTime?: Date;
		endTime?: Date;
	},
): Promise<Array<DashboardServiceLogLine>> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "listServiceLogs", (platform) =>
		platform.listServiceLogs(session.user, input),
	);
}

export async function listServiceDeploymentsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string; limit?: number },
): Promise<Array<DashboardDeploymentRecord>> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "listServiceDeployments", (platform) =>
		platform.listServiceDeployments(session.user, {
			serviceId: input.serviceId,
			limit: input.limit,
		}),
	);
}

export async function updateServiceFromSession(
	runtime: DashboardRuntime,
	input: UpdateServiceInput,
): Promise<DashboardServiceRecord> {
	const session = await requireSession(runtime);
	const current = await platformCall(runtime, "getService", (platform) =>
		platform.getService(session.user, {
			serviceId: input.serviceId,
		}),
	);
	const currentSource = current.spec?.source;
	const repositorySelector =
		input.repositorySelector ?? currentSource?.repositorySelector ?? "";
	const trackedRef = input.trackedRef ?? currentSource?.trackedRef ?? "";
	const dockerfilePath =
		input.dockerfilePath ?? currentSource?.buildRecipe?.dockerfilePath ?? "";
	const contextDir =
		input.contextDir ?? currentSource?.buildRecipe?.contextDir ?? ".";
	const desiredSource: DashboardSourceSpec | undefined =
		repositorySelector.trim()
			? {
					provider: currentSource?.provider ?? "github",
					repositorySelector: normalizeRepositorySelector(repositorySelector),
					trackedRef: trackedRef.trim() || "main",
					buildRecipe: {
						dockerfilePath: dockerfilePath.trim(),
						contextDir: contextDir.trim() || ".",
					},
				}
			: undefined;
	const repositoryChanged =
		desiredSource?.provider.trim().toLowerCase() === "github" &&
		desiredSource.repositorySelector !==
			(currentSource?.repositorySelector
				? normalizeRepositorySelector(currentSource.repositorySelector)
				: "");
	const nextReplicaCount =
		input.desiredReplicaCount ?? current.spec?.desiredReplicaCount;
	if (nextReplicaCount !== undefined && nextReplicaCount < 1) {
		throw new DashboardValidationError({
			message: "Replica count must be at least 1.",
		});
	}
	if (repositoryChanged) {
		const environment = await platformCall(
			runtime,
			"getEnvironment",
			(platform) =>
				platform.getEnvironment(session.user, current.environmentId),
		);
		const githubUserAccessToken = await requireGitHubRepositoryAccess(
			runtime,
			session.user.id,
			desiredSource.repositorySelector,
		);
		await platformCall(runtime, "linkGitHubRepository", (platform) =>
			platform.linkGitHubRepository(session.user, {
				projectId: environment.projectId,
				repositorySelector: desiredSource.repositorySelector,
				githubUserAccessToken,
			}),
		);
	}
	return platformCall(runtime, "updateService", (platform) =>
		platform.updateService(session.user, {
			serviceId: input.serviceId,
			...(input.serviceName?.trim() ? { name: input.serviceName.trim() } : {}),
			spec: {
				...(desiredSource ? { source: desiredSource } : {}),
				desiredReplicaCount:
					input.desiredReplicaCount ?? current.spec?.desiredReplicaCount,
				placementRegion:
					input.placementRegion?.trim().toLowerCase() ??
					current.spec?.placementRegion,
				rollingStrategy: input.rollingStrategy ?? current.spec?.rollingStrategy,
				runtime: {
					env: normalizeRuntimeEnv(
						input.runtimeEnv ?? current.spec?.runtime.env,
					),
					cpuMillis: normalizeResource(
						input.cpuMillis ?? current.spec?.runtime.cpuMillis,
						DEFAULT_SERVICE_CPU_MILLIS,
						"CPU request",
					),
					memoryMebibytes: normalizeResource(
						input.memoryMebibytes ?? current.spec?.runtime.memoryMebibytes,
						DEFAULT_SERVICE_MEMORY_MEBIBYTES,
						"memory request",
					),
					ports: current.spec?.runtime.ports ?? [],
					...(current.spec?.runtime.healthCheck
						? { healthCheck: current.spec.runtime.healthCheck }
						: {}),
					...(current.spec?.runtime.livenessCheck
						? { livenessCheck: current.spec.runtime.livenessCheck }
						: {}),
					...((input.restart ?? current.spec?.runtime.restart)
						? { restart: input.restart ?? current.spec?.runtime.restart }
						: {}),
					...(current.spec?.runtime.volumeName
						? { volumeName: current.spec.runtime.volumeName }
						: {}),
				},
			},
		}),
	);
}

export async function applyDeploymentActionFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		deploymentId: string;
		action: DashboardDeploymentAction;
		idempotencyKey: string;
		allocationId?: string;
	},
): Promise<DashboardServiceStatus> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "applyDeploymentAction", (platform) =>
		platform.applyDeploymentAction(session.user, input),
	);
}

export async function scaleServiceFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		desiredReplicaCount: number;
	},
): Promise<DashboardServiceStatus> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "scaleService", (platform) =>
		platform.scaleService(session.user, {
			serviceId: input.serviceId,
			desiredReplicaCount: input.desiredReplicaCount,
		}),
	);
}

export async function discardServiceChangesFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		changeIds?: Array<string>;
		discardAll?: boolean;
	},
): Promise<DashboardServiceRecord> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "discardServiceChanges", (platform) =>
		platform.discardServiceChanges(session.user, {
			serviceId: input.serviceId,
			changeIds: input.changeIds,
			discardAll: input.discardAll,
		}),
	);
}

export async function deleteServiceFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await platformCall(runtime, "deleteService", (platform) =>
		platform.deleteService(session.user, { serviceId: input.serviceId }),
	);
}

export async function saveServicePositionFromSession(
	runtime: DashboardRuntime,
	input: {
		environmentId: string;
		serviceId: string;
		position: DashboardServicePosition;
	},
): Promise<DashboardServicePosition> {
	const session = await requireSession(runtime);
	const position = normalizeServicePosition(input.position);
	return storeCall(runtime, "saveServicePosition", (store) =>
		store.saveServicePosition(session.user.id, {
			environmentId: input.environmentId,
			serviceId: input.serviceId,
			position,
		}),
	);
}
