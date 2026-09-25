import type { PlatformGateway } from "#/lib/dashboard/core/types.server";
import {
	getOpsClient,
	getPlatformClient,
	type PlatformRuntimeConfig,
	toPlatformGatewayError,
	userAssertionMetadata,
} from "#/lib/platform-grpc/client.server";
import {
	toAgentEnrollment,
	toApplyDeploymentActionRequest,
	toBuildAttempts,
	toCreateAgentRequest,
	toCreateDomainBindingRequest,
	toCreateEnvironmentRequest,
	toCreateProjectRequest,
	toCreateServiceRequest,
	toDeleteDomainBindingRequest,
	toDeleteEnvironmentRequest,
	toDeleteProjectRequest,
	toDeleteServiceRequest,
	toDeleteServiceSecretRequest,
	toDeleteVolumeRequest,
	toDeletionPreview,
	toDeploymentRecords,
	toDiscardServiceChangesRequest,
	toDomainBinding,
	toDomainBindings,
	toDuplicateEnvironmentRequest,
	toEmptyRequest,
	toEnvironment,
	toEnvironments,
	toFleet,
	toFleetAgent,
	toGenerateDomainBindingRequest,
	toGetEnvironmentRequest,
	toGetProjectRequest,
	toGetServiceRequest,
	toGetServiceStatusRequest,
	toIndexedServiceStatus,
	toIndexedServices,
	toIngestGitHubWebhookRequest,
	toInspectSourceRequest,
	toLinkGitHubRepositoryRequest,
	toListBuildAttemptsRequest,
	toListDomainBindingsRequest,
	toListEnvironmentsRequest,
	toListProjectsRequest,
	toListServiceDeploymentsRequest,
	toListServiceLogsRequest,
	toListServiceSecretsRequest,
	toListServicesRequest,
	toListVolumesRequest,
	toPreviewEnvironmentDeletionRequest,
	toPreviewProjectDeletionRequest,
	toPreviewVolumeDeletionRequest,
	toProject,
	toProjects,
	toReleaseEnvironmentRequest,
	toRenameEnvironmentRequest,
	toRepositoryInspection,
	toRestoreDomainBindingRequest,
	toRestoreEnvironmentRequest,
	toRestoreProjectRequest,
	toRestoreServiceRequest,
	toScaleServiceRequest,
	toSealServiceSecretRequest,
	toServiceLogPage,
	toServiceRecord,
	toServiceSecret,
	toServiceSecrets,
	toServiceStatus,
	toSetAgentLifecycleRequest,
	toUpdateAgentRequest,
	toUpdateDomainBindingRequest,
	toUpdateEnvironmentAutoDeployRequest,
	toUpdateProjectLogRetentionRequest,
	toUpdateServiceRequest,
	toVolume,
} from "#/lib/platform-grpc/proto-mappers.server";

export interface IngestGitHubWebhookInput {
	deliveryId: string;
	eventType: string;
	signature256: string;
	payload: Uint8Array;
}

/** Runs one RPC and reports failures as a PlatformGatewayError. */
async function rpc<A>(operation: string, run: () => Promise<A>): Promise<A> {
	try {
		return await run();
	} catch (cause) {
		throw toPlatformGatewayError(operation, cause);
	}
}

export function createPlatformGateway(
	runtime: PlatformRuntimeConfig,
): PlatformGateway {
	const platform = getPlatformClient(runtime);
	const ops = getOpsClient(runtime);

	function callOptions(user: Parameters<typeof userAssertionMetadata>[1]) {
		return { headers: userAssertionMetadata(runtime, user) };
	}

	return {
		listFleet: (user) =>
			rpc("ListFleet", async () =>
				toFleet(await ops.listFleet(toEmptyRequest(), callOptions(user))),
			),
		createFleetAgent: (user, input) =>
			rpc("CreateAgent", async () =>
				toAgentEnrollment(
					await ops.createAgent(toCreateAgentRequest(input), callOptions(user)),
				),
			),
		updateFleetAgent: (user, input) =>
			rpc("UpdateAgent", async () =>
				toFleetAgent(
					await ops.updateAgent(toUpdateAgentRequest(input), callOptions(user)),
				),
			),
		setFleetAgentLifecycle: (user, input) =>
			rpc("SetAgentLifecycle", async () =>
				toFleetAgent(
					await ops.setAgentLifecycle(
						toSetAgentLifecycleRequest(input),
						callOptions(user),
					),
				),
			),
		listProjects: (user, options) =>
			rpc("ListProjects", async () =>
				toProjects(
					(
						await platform.listProjects(
							toListProjectsRequest(options?.includeDeleted),
							callOptions(user),
						)
					).projects,
				),
			),
		getProject: (user, projectId) =>
			rpc("GetProject", async () =>
				toProject(
					await platform.getProject(
						toGetProjectRequest(projectId),
						callOptions(user),
					),
				),
			),
		createProject: (user, name) =>
			rpc("CreateProject", async () =>
				toProject(
					await platform.createProject(
						toCreateProjectRequest(name),
						callOptions(user),
					),
				),
			),
		updateProjectLogRetention: (user, input) =>
			rpc("UpdateProjectLogRetention", async () =>
				toProject(
					await platform.updateProjectLogRetention(
						toUpdateProjectLogRetentionRequest(input),
						callOptions(user),
					),
				),
			),
		previewProjectDeletion: (user, projectId) =>
			rpc("PreviewProjectDeletion", async () =>
				toDeletionPreview(
					await platform.previewProjectDeletion(
						toPreviewProjectDeletionRequest(projectId),
						callOptions(user),
					),
				),
			),
		deleteProject: (user, input) =>
			rpc("DeleteProject", async () => {
				await platform.deleteProject(
					toDeleteProjectRequest(input),
					callOptions(user),
				);
			}),
		restoreProject: (user, projectId) =>
			rpc("RestoreProject", async () =>
				toProject(
					await platform.restoreProject(
						toRestoreProjectRequest(projectId),
						callOptions(user),
					),
				),
			),
		listEnvironments: (user, projectId, options) =>
			rpc("ListEnvironments", async () =>
				toEnvironments(
					(
						await platform.listEnvironments(
							toListEnvironmentsRequest(projectId, options?.includeDeleted),
							callOptions(user),
						)
					).environments,
				),
			),
		getEnvironment: (user, environmentId) =>
			rpc("GetEnvironment", async () =>
				toEnvironment(
					await platform.getEnvironment(
						toGetEnvironmentRequest(environmentId),
						callOptions(user),
					),
				),
			),
		createEnvironment: (user, input) =>
			rpc("CreateEnvironment", async () =>
				toEnvironment(
					await platform.createEnvironment(
						toCreateEnvironmentRequest(input),
						callOptions(user),
					),
				),
			),
		duplicateEnvironment: (user, input) =>
			rpc("DuplicateEnvironment", async () =>
				toEnvironment(
					await platform.duplicateEnvironment(
						toDuplicateEnvironmentRequest(input),
						callOptions(user),
					),
				),
			),
		renameEnvironment: (user, input) =>
			rpc("RenameEnvironment", async () =>
				toEnvironment(
					await platform.renameEnvironment(
						toRenameEnvironmentRequest(input),
						callOptions(user),
					),
				),
			),
		updateEnvironmentAutoDeploy: (user, input) =>
			rpc("UpdateEnvironmentAutoDeploy", async () =>
				toEnvironment(
					await platform.updateEnvironmentAutoDeploy(
						toUpdateEnvironmentAutoDeployRequest(input),
						callOptions(user),
					),
				),
			),
		previewEnvironmentDeletion: (user, environmentId) =>
			rpc("PreviewEnvironmentDeletion", async () =>
				toDeletionPreview(
					await platform.previewEnvironmentDeletion(
						toPreviewEnvironmentDeletionRequest(environmentId),
						callOptions(user),
					),
				),
			),
		deleteEnvironment: (user, input) =>
			rpc("DeleteEnvironment", async () => {
				await platform.deleteEnvironment(
					toDeleteEnvironmentRequest(input),
					callOptions(user),
				);
			}),
		restoreEnvironment: (user, environmentId) =>
			rpc("RestoreEnvironment", async () =>
				toEnvironment(
					await platform.restoreEnvironment(
						toRestoreEnvironmentRequest(environmentId),
						callOptions(user),
					),
				),
			),
		releaseEnvironment: (user, environmentId) =>
			rpc("ReleaseEnvironment", async () => {
				const response = await platform.releaseEnvironment(
					toReleaseEnvironmentRequest(environmentId),
					callOptions(user),
				);
				return response.services.map(toServiceStatus);
			}),
		listServices: (user, environmentId, options) =>
			rpc("ListServices", async () =>
				toIndexedServices(
					await platform.listServices(
						toListServicesRequest({
							environmentId,
							includeDeleted: options?.includeDeleted,
						}),
						callOptions(user),
					),
				),
			),
		waitForServices: (user, input) =>
			rpc("ListServices", async () =>
				toIndexedServices(
					await platform.listServices(
						toListServicesRequest(input),
						callOptions(user),
					),
				),
			),
		inspectRepositorySource: (user, input) =>
			rpc("InspectSource", async () =>
				toRepositoryInspection(
					await platform.inspectSource(
						toInspectSourceRequest(input),
						callOptions(user),
					),
				),
			),
		linkGitHubRepository: (user, input) =>
			rpc("LinkGitHubRepository", async () =>
				toRepositoryInspection(
					await platform.linkGitHubRepository(
						toLinkGitHubRepositoryRequest(input),
						callOptions(user),
					),
				),
			),
		createService: (user, input) =>
			rpc("CreateService", async () =>
				toServiceRecord(
					await platform.createService(
						toCreateServiceRequest(input),
						callOptions(user),
					),
				),
			),
		updateService: (user, input) =>
			rpc("UpdateService", async () =>
				toServiceRecord(
					await platform.updateService(
						toUpdateServiceRequest(input),
						callOptions(user),
					),
				),
			),
		applyDeploymentAction: (user, input) =>
			rpc("ApplyDeploymentAction", async () =>
				toServiceStatus(
					await platform.applyDeploymentAction(
						toApplyDeploymentActionRequest(input),
						callOptions(user),
					),
				),
			),
		scaleService: (user, input) =>
			rpc("ScaleService", async () =>
				toServiceStatus(
					await platform.scaleService(
						toScaleServiceRequest(input),
						callOptions(user),
					),
				),
			),
		discardServiceChanges: (user, input) =>
			rpc("DiscardServiceChanges", async () =>
				toServiceRecord(
					await platform.discardServiceChanges(
						toDiscardServiceChangesRequest(input),
						callOptions(user),
					),
				),
			),
		deleteService: (user, input) =>
			rpc("DeleteService", async () => {
				await platform.deleteService(
					toDeleteServiceRequest(input.serviceId),
					callOptions(user),
				);
			}),
		restoreService: (user, serviceId) =>
			rpc("RestoreService", async () =>
				toServiceRecord(
					await platform.restoreService(
						toRestoreServiceRequest(serviceId),
						callOptions(user),
					),
				),
			),
		getService: (user, input) =>
			rpc("GetService", async () =>
				toServiceRecord(
					await platform.getService(
						toGetServiceRequest(input.serviceId),
						callOptions(user),
					),
				),
			),
		getServiceStatus: (user, input) =>
			rpc("GetServiceStatus", async () =>
				toServiceStatus(
					await platform.getServiceStatus(
						toGetServiceStatusRequest(input),
						callOptions(user),
					),
				),
			),
		waitForServiceStatus: (user, input) =>
			rpc("GetServiceStatus", async () =>
				toIndexedServiceStatus(
					await platform.getServiceStatus(
						toGetServiceStatusRequest(input),
						callOptions(user),
					),
				),
			),
		listServiceSecrets: (user, serviceId) =>
			rpc("ListServiceSecrets", async () =>
				toServiceSecrets(
					await platform.listServiceSecrets(
						toListServiceSecretsRequest(serviceId),
						callOptions(user),
					),
				),
			),
		sealServiceSecret: (user, input) =>
			rpc("SealServiceSecret", async () =>
				toServiceSecret(
					await platform.sealServiceSecret(
						toSealServiceSecretRequest(input),
						callOptions(user),
					),
				),
			),
		deleteServiceSecret: (user, input) =>
			rpc("DeleteServiceSecret", async () => {
				await platform.deleteServiceSecret(
					toDeleteServiceSecretRequest(input),
					callOptions(user),
				);
			}),
		listVolumes: (user, environmentId, options) =>
			rpc("ListVolumes", async () =>
				(
					await platform.listVolumes(
						toListVolumesRequest(environmentId, options?.includeDeleted),
						callOptions(user),
					)
				).volumes.map(toVolume),
			),
		previewVolumeDeletion: (user, volumeId) =>
			rpc("PreviewVolumeDeletion", async () =>
				toDeletionPreview(
					await platform.previewVolumeDeletion(
						toPreviewVolumeDeletionRequest(volumeId),
						callOptions(user),
					),
				),
			),
		deleteVolume: (user, input) =>
			rpc("DeleteVolume", async () => {
				await platform.deleteVolume(
					toDeleteVolumeRequest(input),
					callOptions(user),
				);
			}),
		listServiceLogs: (user, input) =>
			rpc("ListServiceLogs", async () =>
				toServiceLogPage(
					await platform.listServiceLogs(
						toListServiceLogsRequest(input),
						callOptions(user),
					),
				),
			),
		listServiceDeployments: (user, input) =>
			rpc("ListServiceDeployments", async () =>
				toDeploymentRecords(
					await platform.listServiceDeployments(
						toListServiceDeploymentsRequest(input),
						callOptions(user),
					),
				),
			),
		listBuildAttempts: (user, input) =>
			rpc("ListBuildAttempts", async () =>
				toBuildAttempts(
					await platform.listBuildAttempts(
						toListBuildAttemptsRequest(input),
						callOptions(user),
					),
				),
			),
		listDomainBindings: (user, input) =>
			rpc("ListDomainBindings", async () =>
				toDomainBindings(
					(
						await platform.listDomainBindings(
							toListDomainBindingsRequest(input),
							callOptions(user),
						)
					).bindings,
				),
			),
		generateDomainBinding: (user, input) =>
			rpc("GenerateDomainBinding", async () =>
				toDomainBinding(
					await platform.generateDomainBinding(
						toGenerateDomainBindingRequest(input),
						callOptions(user),
					),
				),
			),
		createDomainBinding: (user, input) =>
			rpc("CreateDomainBinding", async () =>
				toDomainBinding(
					await platform.createDomainBinding(
						toCreateDomainBindingRequest(input),
						callOptions(user),
					),
				),
			),
		updateDomainBinding: (user, input) =>
			rpc("UpdateDomainBinding", async () =>
				toDomainBinding(
					await platform.updateDomainBinding(
						toUpdateDomainBindingRequest(input),
						callOptions(user),
					),
				),
			),
		deleteDomainBinding: (user, input) =>
			rpc("DeleteDomainBinding", async () => {
				await platform.deleteDomainBinding(
					toDeleteDomainBindingRequest(input.hostname),
					callOptions(user),
				);
			}),
		restoreDomainBinding: (user, hostname) =>
			rpc("RestoreDomainBinding", async () =>
				toDomainBinding(
					await platform.restoreDomainBinding(
						toRestoreDomainBindingRequest(hostname),
						callOptions(user),
					),
				),
			),
	};
}

export function ingestGitHubWebhook(
	runtime: PlatformRuntimeConfig,
	input: IngestGitHubWebhookInput,
): Promise<void> {
	return rpc("IngestGitHubWebhook", async () => {
		await getOpsClient(runtime).ingestGitHubWebhook(
			toIngestGitHubWebhookRequest(input),
		);
	});
}
