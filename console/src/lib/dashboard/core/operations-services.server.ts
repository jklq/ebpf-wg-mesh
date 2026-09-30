import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	storeCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardBuildAttempt,
	type DashboardDeploymentAction,
	type DashboardDeploymentRecord,
	type DashboardServiceLogPage,
	type DashboardServiceLogType,
	type DashboardServicePosition,
	type DashboardServiceRecord,
	type DashboardServiceSecret,
	type DashboardServiceStatus,
	type DashboardSourceSpec,
	DashboardValidationError,
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
	type UpdateServiceInput,
} from "#/lib/dashboard/core/types.server";
import { normalizeRepositorySelector } from "#/lib/dashboard/onboarding/flow";
import { sealedSecretName } from "#/lib/dashboard/sealed-secrets";
import { PlatformService } from "#/lib/platform-gen/platform_pb";
import { integerString } from "#/lib/platform-json";
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
		waitIndex: string;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	return runtime.platform
		.call(PlatformService.method.getServiceStatus, session.user, input)
		.then((status) => ({
			index: status.index,
			notModified: status.notModified,
			status: status.notModified ? undefined : status,
		}));
}

export async function listEnvironmentServicesFromSession(
	runtime: DashboardRuntime,
	input: { environmentId: string },
) {
	const session = await requireSession(runtime);
	const [snapshot, positions] = await Promise.all([
		runtime.platform.call(PlatformService.method.listServices, session.user, {
			environmentId: input.environmentId,
		}),
		storeCall(runtime, "listServicePositions", (store) =>
			store.listServicePositions(session.user.id, input.environmentId),
		),
	]);
	return {
		...snapshot,
		services: snapshot.services
			? applyServicePositions(snapshot.services, positions)
			: undefined,
	};
}

export async function waitForEnvironmentServicesFromSession(
	runtime: DashboardRuntime,
	input: {
		environmentId: string;
		waitIndex: string;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	const result = await runtime.platform.call(
		PlatformService.method.listServices,
		session.user,
		input,
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
		pageToken?: string;
		gapPageToken?: string;
	},
): Promise<DashboardServiceLogPage> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.listServiceLogs,
		session.user,
		{
			...input,
			startTime: input.startTime?.toISOString(),
			endTime: input.endTime?.toISOString(),
		},
	);
}

export async function listServiceDeploymentsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string; limit?: number },
): Promise<Array<DashboardDeploymentRecord>> {
	const session = await requireSession(runtime);
	return runtime.platform
		.call(PlatformService.method.listServiceDeployments, session.user, {
			serviceId: input.serviceId,
			limit: input.limit,
		})
		.then((response) => response.deployments ?? []);
}

export async function listBuildAttemptsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string; buildId: string },
): Promise<Array<DashboardBuildAttempt>> {
	const session = await requireSession(runtime);
	return runtime.platform
		.call(PlatformService.method.listBuildAttempts, session.user, input)
		.then((response) => response.attempts ?? []);
}

export async function listServiceSecretsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<Array<DashboardServiceSecret>> {
	const session = await requireSession(runtime);
	return runtime.platform
		.call(PlatformService.method.listServiceSecrets, session.user, {
			serviceId: input.serviceId,
		})
		.then((response) => response.secrets ?? []);
}

/** Seals a write-only value. The value goes to the RPC and nowhere else. */
export async function sealServiceSecretFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string; name: string; value: string },
): Promise<DashboardServiceSecret> {
	const session = await requireSession(runtime);
	const name = sealedSecretName.safeParse(input.name);
	if (!name.success) {
		throw new DashboardValidationError({
			message: name.error.issues[0]?.message ?? "Invalid secret name.",
		});
	}
	return runtime.platform.call(
		PlatformService.method.sealServiceSecret,
		session.user,
		{
			serviceId: input.serviceId,
			name: name.data,
			value: input.value,
		},
	);
}

export async function deleteServiceSecretFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string; name: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await runtime.platform.call(
		PlatformService.method.deleteServiceSecret,
		session.user,
		input,
	);
}

export async function updateServiceFromSession(
	runtime: DashboardRuntime,
	input: UpdateServiceInput,
): Promise<DashboardServiceRecord> {
	const session = await requireSession(runtime);
	const current = await runtime.platform.call(
		PlatformService.method.getService,
		session.user,
		{
			serviceId: input.serviceId,
		},
	);
	const currentSource = current.spec?.source?.sourceSpec;
	const repositorySelector =
		input.repositorySelector ?? currentSource?.repositorySelector ?? "";
	const trackedRef = input.trackedRef ?? currentSource?.trackedRef ?? "";
	const builder =
		input.builder ??
		currentSource?.buildRecipe?.builder ??
		"BUILDER_KIND_RAILPACK";
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
						builder,
						dockerfilePath:
							builder === "BUILDER_KIND_RAILPACK" ? "" : dockerfilePath.trim(),
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
		const environment = await runtime.platform.call(
			PlatformService.method.getEnvironment,
			session.user,
			{ environmentId: current.environmentId },
		);
		const githubUserAccessToken = await requireGitHubRepositoryAccess(
			runtime,
			session.user.id,
			desiredSource.repositorySelector,
		);
		await runtime.platform.call(
			PlatformService.method.linkGitHubRepository,
			session.user,
			{
				projectId: environment.projectId,
				repositorySelector: desiredSource.repositorySelector,
				githubUserAccessToken,
			},
		);
	}
	return runtime.platform.call(
		PlatformService.method.updateService,
		session.user,
		{
			serviceId: input.serviceId,
			service: {
				...(input.serviceName?.trim()
					? { name: input.serviceName.trim() }
					: {}),
				spec: {
					...current.spec,
					source: desiredSource
						? { sourceSpec: desiredSource }
						: current.spec?.source,
					desiredReplicaCount:
						input.desiredReplicaCount ?? current.spec?.desiredReplicaCount,
					placementRegion:
						input.placementRegion?.trim().toLowerCase() ??
						current.spec?.placementRegion,
					rollingStrategy:
						input.rollingStrategy ?? current.spec?.rollingStrategy,
					runtime: {
						...current.spec?.runtime,
						env:
							input.runtimeEnv === undefined
								? (current.spec?.runtime?.env ?? {})
								: normalizeRuntimeEnv(input.runtimeEnv),
						cpuMillis:
							input.cpuMillis === undefined
								? (current.spec?.runtime?.cpuMillis ??
									integerString(DEFAULT_SERVICE_CPU_MILLIS))
								: integerString(
										normalizeResource(
											input.cpuMillis,
											DEFAULT_SERVICE_CPU_MILLIS,
											"CPU request",
										),
									),
						memoryMebibytes:
							input.memoryMebibytes === undefined
								? (current.spec?.runtime?.memoryMebibytes ??
									integerString(DEFAULT_SERVICE_MEMORY_MEBIBYTES))
								: integerString(
										normalizeResource(
											input.memoryMebibytes,
											DEFAULT_SERVICE_MEMORY_MEBIBYTES,
											"memory request",
										),
									),
						...(input.restart
							? {
									restart: {
										...current.spec?.runtime?.restart,
										...input.restart,
									},
								}
							: {}),
					},
				},
			},
		},
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
	return runtime.platform.call(
		PlatformService.method.applyDeploymentAction,
		session.user,
		input,
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
	return runtime.platform.call(
		PlatformService.method.scaleService,
		session.user,
		{
			serviceId: input.serviceId,
			desiredReplicaCount: input.desiredReplicaCount,
		},
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
	return runtime.platform.call(
		PlatformService.method.discardServiceChanges,
		session.user,
		{
			serviceId: input.serviceId,
			changeIds: input.changeIds,
			discardAll: input.discardAll,
		},
	);
}

export async function deleteServiceFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await runtime.platform.call(
		PlatformService.method.deleteService,
		session.user,
		{ serviceId: input.serviceId },
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
