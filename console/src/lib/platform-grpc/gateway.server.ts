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
	toCreateAgentRequest,
	toCreateDomainBindingRequest,
	toCreateEnvironmentRequest,
	toCreateProjectRequest,
	toCreateServiceRequest,
	toDeleteDomainBindingRequest,
	toDeleteEnvironmentRequest,
	toDeleteServiceRequest,
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
	toGetServiceRequest,
	toGetServiceStatusRequest,
	toIndexedServiceStatus,
	toIndexedServices,
	toIngestGitHubWebhookRequest,
	toInspectSourceRequest,
	toLinkGitHubRepositoryRequest,
	toListDomainBindingsRequest,
	toListEnvironmentsRequest,
	toListServiceDeploymentsRequest,
	toListServiceLogsRequest,
	toListServicesRequest,
	toProject,
	toProjects,
	toReleaseEnvironmentRequest,
	toRenameEnvironmentRequest,
	toRepositoryInspection,
	toScaleServiceRequest,
	toServiceLogLines,
	toServiceRecord,
	toServiceStatus,
	toSetAgentLifecycleRequest,
	toUpdateAgentRequest,
	toUpdateDomainBindingRequest,
	toUpdateServiceRequest,
} from "#/lib/platform-grpc/proto-mappers.server";

export interface IngestGitHubWebhookInput {
	deliveryId: string;
	eventType: string;
	signature256: string;
	payload: Uint8Array;
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
		async listFleet(user) {
			try {
				return toFleet(
					await ops.listFleet(toEmptyRequest(), callOptions(user)),
				);
			} catch (cause) {
				throw toPlatformGatewayError("ListFleet", cause);
			}
		},
		async createFleetAgent(user, input) {
			try {
				return toAgentEnrollment(
					await ops.createAgent(toCreateAgentRequest(input), callOptions(user)),
				);
			} catch (cause) {
				throw toPlatformGatewayError("CreateAgent", cause);
			}
		},
		async updateFleetAgent(user, input) {
			try {
				return toFleetAgent(
					await ops.updateAgent(toUpdateAgentRequest(input), callOptions(user)),
				);
			} catch (cause) {
				throw toPlatformGatewayError("UpdateAgent", cause);
			}
		},
		async setFleetAgentLifecycle(user, input) {
			try {
				return toFleetAgent(
					await ops.setAgentLifecycle(
						toSetAgentLifecycleRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("SetAgentLifecycle", cause);
			}
		},
		async listProjects(user) {
			try {
				return toProjects(
					(await platform.listProjects(toEmptyRequest(), callOptions(user)))
						.projects,
				);
			} catch (cause) {
				throw toPlatformGatewayError("ListProjects", cause);
			}
		},
		async createProject(user, name) {
			try {
				return toProject(
					await platform.createProject(
						toCreateProjectRequest(name),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("CreateProject", cause);
			}
		},
		async listEnvironments(user, projectId) {
			try {
				return toEnvironments(
					(
						await platform.listEnvironments(
							toListEnvironmentsRequest(projectId),
							callOptions(user),
						)
					).environments,
				);
			} catch (cause) {
				throw toPlatformGatewayError("ListEnvironments", cause);
			}
		},
		async getEnvironment(user, environmentId) {
			try {
				return toEnvironment(
					await platform.getEnvironment(
						toGetEnvironmentRequest(environmentId),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("GetEnvironment", cause);
			}
		},
		async createEnvironment(user, input) {
			try {
				return toEnvironment(
					await platform.createEnvironment(
						toCreateEnvironmentRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("CreateEnvironment", cause);
			}
		},
		async duplicateEnvironment(user, input) {
			try {
				return toEnvironment(
					await platform.duplicateEnvironment(
						toDuplicateEnvironmentRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("DuplicateEnvironment", cause);
			}
		},
		async renameEnvironment(user, input) {
			try {
				return toEnvironment(
					await platform.renameEnvironment(
						toRenameEnvironmentRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("RenameEnvironment", cause);
			}
		},
		async deleteEnvironment(user, environmentId) {
			try {
				await platform.deleteEnvironment(
					toDeleteEnvironmentRequest(environmentId),
					callOptions(user),
				);
			} catch (cause) {
				throw toPlatformGatewayError("DeleteEnvironment", cause);
			}
		},
		async releaseEnvironment(user, environmentId) {
			try {
				const response = await platform.releaseEnvironment(
					toReleaseEnvironmentRequest(environmentId),
					callOptions(user),
				);
				return response.services.map(toServiceStatus);
			} catch (cause) {
				throw toPlatformGatewayError("ReleaseEnvironment", cause);
			}
		},
		async listServices(user, environmentId) {
			try {
				const response = await platform.listServices(
					toListServicesRequest({ environmentId }),
					callOptions(user),
				);
				return response.services.map(toServiceRecord);
			} catch (cause) {
				throw toPlatformGatewayError("ListServices", cause);
			}
		},
		async waitForServices(user, input) {
			try {
				return toIndexedServices(
					await platform.listServices(
						toListServicesRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("ListServices", cause);
			}
		},
		async inspectRepositorySource(user, input) {
			try {
				return toRepositoryInspection(
					await platform.inspectSource(
						toInspectSourceRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("InspectSource", cause);
			}
		},
		async linkGitHubRepository(user, input) {
			try {
				return toRepositoryInspection(
					await platform.linkGitHubRepository(
						toLinkGitHubRepositoryRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("LinkGitHubRepository", cause);
			}
		},
		async createService(user, input) {
			try {
				return toServiceRecord(
					await platform.createService(
						toCreateServiceRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("CreateService", cause);
			}
		},
		async updateService(user, input) {
			try {
				return toServiceRecord(
					await platform.updateService(
						toUpdateServiceRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("UpdateService", cause);
			}
		},
		async applyDeploymentAction(user, input) {
			try {
				return toServiceStatus(
					await platform.applyDeploymentAction(
						toApplyDeploymentActionRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("ApplyDeploymentAction", cause);
			}
		},
		async scaleService(user, input) {
			try {
				return toServiceStatus(
					await platform.scaleService(
						toScaleServiceRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("ScaleService", cause);
			}
		},
		async discardServiceChanges(user, input) {
			try {
				return toServiceRecord(
					await platform.discardServiceChanges(
						toDiscardServiceChangesRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("DiscardServiceChanges", cause);
			}
		},
		async deleteService(user, input) {
			try {
				await platform.deleteService(
					toDeleteServiceRequest(input.serviceId),
					callOptions(user),
				);
			} catch (cause) {
				throw toPlatformGatewayError("DeleteService", cause);
			}
		},
		async getService(user, input) {
			try {
				return toServiceRecord(
					await platform.getService(
						toGetServiceRequest(input.serviceId),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("GetService", cause);
			}
		},
		async getServiceStatus(user, input) {
			try {
				return toServiceStatus(
					await platform.getServiceStatus(
						toGetServiceStatusRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("GetServiceStatus", cause);
			}
		},
		async waitForServiceStatus(user, input) {
			try {
				return toIndexedServiceStatus(
					await platform.getServiceStatus(
						toGetServiceStatusRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("GetServiceStatus", cause);
			}
		},
		async listServiceLogs(user, input) {
			try {
				return toServiceLogLines(
					await platform.listServiceLogs(
						toListServiceLogsRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("ListServiceLogs", cause);
			}
		},
		async listServiceDeployments(user, input) {
			try {
				return toDeploymentRecords(
					await platform.listServiceDeployments(
						toListServiceDeploymentsRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("ListServiceDeployments", cause);
			}
		},
		async listDomainBindings(user, input) {
			try {
				return toDomainBindings(
					(
						await platform.listDomainBindings(
							toListDomainBindingsRequest(input.serviceId),
							callOptions(user),
						)
					).bindings,
				);
			} catch (cause) {
				throw toPlatformGatewayError("ListDomainBindings", cause);
			}
		},
		async generateDomainBinding(user, input) {
			try {
				return toDomainBinding(
					await platform.generateDomainBinding(
						toGenerateDomainBindingRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("GenerateDomainBinding", cause);
			}
		},
		async createDomainBinding(user, input) {
			try {
				return toDomainBinding(
					await platform.createDomainBinding(
						toCreateDomainBindingRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("CreateDomainBinding", cause);
			}
		},
		async updateDomainBinding(user, input) {
			try {
				return toDomainBinding(
					await platform.updateDomainBinding(
						toUpdateDomainBindingRequest(input),
						callOptions(user),
					),
				);
			} catch (cause) {
				throw toPlatformGatewayError("UpdateDomainBinding", cause);
			}
		},
		async deleteDomainBinding(user, input) {
			try {
				await platform.deleteDomainBinding(
					toDeleteDomainBindingRequest(input.hostname),
					callOptions(user),
				);
			} catch (cause) {
				throw toPlatformGatewayError("DeleteDomainBinding", cause);
			}
		},
	};
}

export async function ingestGitHubWebhook(
	runtime: PlatformRuntimeConfig,
	input: IngestGitHubWebhookInput,
): Promise<void> {
	try {
		await getOpsClient(runtime).ingestGitHubWebhook(
			toIngestGitHubWebhookRequest(input),
		);
	} catch (cause) {
		throw toPlatformGatewayError("IngestGitHubWebhook", cause);
	}
}
