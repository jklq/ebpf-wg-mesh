import * as grpc from "@grpc/grpc-js";

import type { PlatformGateway } from "#/lib/dashboard/core/types.server";
import {
	getOpsClient,
	opsUserCall,
	toPlatformGatewayError,
	unaryCall,
} from "#/lib/platform-grpc/client.server";
import {
	decodeDomainBindingMessage,
	decodeEnvironmentMessage,
	decodeIndexedServiceStatusResponse,
	decodeIndexedServicesResponse,
	decodeInspectSourceResponse,
	decodeListDomainBindingsResponse,
	decodeListEnvironmentsResponse,
	decodeListProjectsResponse,
	decodeListServiceDeploymentsResponse,
	decodeListServiceLogsResponse,
	decodeListServicesResponse,
	decodeAgentEnrollmentMessage,
	decodeFleetAgentMessage,
	decodeFleetMessage,
	decodeProjectMessage,
	decodeServiceMessage,
	decodeServiceStatusMessage,
	encodeCreateServiceRequest,
	encodeIngestGitHubWebhookRequest,
	encodeListServiceLogsRequest,
	encodeUpdateServiceRequest,
} from "#/lib/platform-grpc/codec.server";
import type {
	IngestGitHubWebhookInput,
	PlatformRuntimeConfig,
} from "#/lib/platform-grpc/types.server";

export function createPlatformGateway(
	runtime: PlatformRuntimeConfig,
): PlatformGateway {
	return {
		async listFleet(user) {
			return decodeFleetMessage(await opsUserCall(runtime, "ListFleet", {}, user));
		},
		async createFleetAgent(user, input) {
			return decodeAgentEnrollmentMessage(
				await opsUserCall(runtime, "CreateAgent", input, user),
			);
		},
		async updateFleetAgent(user, input) {
			return decodeFleetAgentMessage(
				await opsUserCall(runtime, "UpdateAgent", input, user),
			);
		},
		async setFleetAgentLifecycle(user, input) {
			return decodeFleetAgentMessage(
				await opsUserCall(
					runtime,
					"SetAgentLifecycle",
					{
						agentId: input.agentId,
						lifecycleState: encodeAgentLifecycleState(input.lifecycleState),
					},
					user,
				),
			);
		},
		async listProjects(user) {
			const response = await unaryCall(runtime, "ListProjects", {}, user);
			return decodeListProjectsResponse(response).projects;
		},
		async createProject(user, name) {
			const response = await unaryCall(
				runtime,
				"CreateProject",
				{ name },
				user,
			);
			return decodeProjectMessage(response);
		},
		async listEnvironments(user, projectId) {
			const response = await unaryCall(
				runtime,
				"ListEnvironments",
				{ projectId },
				user,
			);
			return decodeListEnvironmentsResponse(response).environments;
		},
		async getEnvironment(user, environmentId) {
			return decodeEnvironmentMessage(
				await unaryCall(runtime, "GetEnvironment", { environmentId }, user),
			);
		},
		async createEnvironment(user, input) {
			return decodeEnvironmentMessage(
				await unaryCall(runtime, "CreateEnvironment", input, user),
			);
		},
		async duplicateEnvironment(user, input) {
			return decodeEnvironmentMessage(
				await unaryCall(runtime, "DuplicateEnvironment", input, user),
			);
		},
		async renameEnvironment(user, input) {
			return decodeEnvironmentMessage(
				await unaryCall(runtime, "RenameEnvironment", input, user),
			);
		},
		async deleteEnvironment(user, environmentId) {
			await unaryCall(runtime, "DeleteEnvironment", { environmentId }, user);
		},
		async deployEnvironment(user, environmentId) {
			const response = (await unaryCall(
				runtime,
				"DeployEnvironment",
				{ environmentId },
				user,
			)) as { services?: unknown[] };
			return (response.services ?? []).map(decodeServiceStatusMessage);
		},
		async listServices(user, environmentId) {
			const response = await unaryCall(
				runtime,
				"ListServices",
				{ environmentId },
				user,
			);
			return decodeListServicesResponse(response);
		},
		async waitForServices(user, input) {
			const response = await unaryCall(runtime, "ListServices", input, user);
			return decodeIndexedServicesResponse(response);
		},
		async inspectRepositorySource(user, input) {
			const response = await unaryCall(
				runtime,
				"InspectSource",
				{
					projectId: input.projectId,
					provider: input.provider,
					repositorySelector: input.repositorySelector,
					githubUserAccessToken: input.githubUserAccessToken,
				},
				user,
			);
			return decodeInspectSourceResponse(response);
		},
		async linkGitHubRepository(user, input) {
			const response = await unaryCall(
				runtime,
				"LinkGitHubRepository",
				input,
				user,
			);
			return decodeInspectSourceResponse(response);
		},
		async createService(user, input) {
			const response = await unaryCall(
				runtime,
				"CreateService",
				encodeCreateServiceRequest(input),
				user,
			);
			return decodeServiceMessage(response);
		},
		async updateService(user, input) {
			const response = await unaryCall(
				runtime,
				"UpdateService",
				encodeUpdateServiceRequest(input),
				user,
			);
			return decodeServiceMessage(response);
		},
		async redeployService(user, input) {
			const response = await unaryCall(runtime, "RedeployService", input, user);
			return decodeServiceStatusMessage(response);
		},
		async scaleService(user, input) {
			const response = await unaryCall(runtime, "ScaleService", input, user);
			return decodeServiceStatusMessage(response);
		},
		async restartService(user, input) {
			const response = await unaryCall(runtime, "RestartService", input, user);
			return decodeServiceStatusMessage(response);
		},
		async discardServiceChanges(user, input) {
			const response = await unaryCall(
				runtime,
				"DiscardServiceChanges",
				input,
				user,
			);
			return decodeServiceMessage(response);
		},
		async deleteService(user, input) {
			await unaryCall(runtime, "DeleteService", input, user);
		},
		async getService(user, input) {
			const response = await unaryCall(runtime, "GetService", input, user);
			return decodeServiceMessage(response);
		},
		async getServiceStatus(user, input) {
			const response = await unaryCall(
				runtime,
				"GetServiceStatus",
				input,
				user,
			);
			return decodeServiceStatusMessage(response);
		},
		async waitForServiceStatus(user, input) {
			const response = await unaryCall(
				runtime,
				"GetServiceStatus",
				input,
				user,
			);
			return decodeIndexedServiceStatusResponse(response);
		},
		async listServiceLogs(user, input) {
			const response = await unaryCall(
				runtime,
				"ListServiceLogs",
				encodeListServiceLogsRequest(input),
				user,
			);
			return decodeListServiceLogsResponse(response).lines.map(
				({ environmentId: _environmentId, ...line }) => line,
			);
		},
		async listServiceDeployments(user, input) {
			const response = await unaryCall(
				runtime,
				"ListServiceDeployments",
				input,
				user,
			);
			return decodeListServiceDeploymentsResponse(response).deployments;
		},
		async listDomainBindings(user, input) {
			const response = await unaryCall(
				runtime,
				"ListDomainBindings",
				input,
				user,
			);
			return decodeListDomainBindingsResponse(response);
		},
		async generateDomainBinding(user, input) {
			const response = await unaryCall(
				runtime,
				"GenerateDomainBinding",
				input,
				user,
			);
			return decodeDomainBindingMessage(response);
		},
		async createDomainBinding(user, input) {
			const response = await unaryCall(
				runtime,
				"CreateDomainBinding",
				{
					binding: {
						hostname: input.hostname,
						serviceId: input.serviceId,
						targetPort: input.targetPort,
					},
				},
				user,
			);
			return decodeDomainBindingMessage(response);
		},
		async updateDomainBinding(user, input) {
			const response = await unaryCall(
				runtime,
				"UpdateDomainBinding",
				{
					hostname: input.hostname,
					binding: {
						serviceId: input.serviceId,
						targetPort: input.targetPort,
					},
				},
				user,
			);
			return decodeDomainBindingMessage(response);
		},
		async deleteDomainBinding(user, input) {
			await unaryCall(
				runtime,
				"DeleteDomainBinding",
				{ hostname: input.hostname },
				user,
			);
		},
	};
}

function encodeAgentLifecycleState(state: string): string {
	return `AGENT_LIFECYCLE_STATE_${state.toUpperCase()}`;
}

export async function ingestGitHubWebhook(
	runtime: PlatformRuntimeConfig,
	input: IngestGitHubWebhookInput,
): Promise<void> {
	try {
		await new Promise<void>((resolve, reject) => {
			getOpsClient(runtime).IngestGitHubWebhook(
				encodeIngestGitHubWebhookRequest(input),
				new grpc.Metadata(),
				(error) => {
					if (error) {
						reject(error);
						return;
					}
					resolve();
				},
			);
		});
	} catch (cause) {
		throw toPlatformGatewayError("IngestGitHubWebhook", cause);
	}
}
