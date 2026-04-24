import * as grpc from "@grpc/grpc-js";

import type { PlatformGateway } from "#/lib/dashboard/core/types.server";
import {
	getOpsClient,
	toPlatformGatewayError,
	unaryCall,
} from "#/lib/platform-grpc/client.server";
import {
	decodeDomainBindingMessage,
	decodeInspectSourceResponse,
	decodeListDomainBindingsResponse,
	decodeListProjectsResponse,
	decodeListServiceLogsResponse,
	decodeListServicesResponse,
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
		async ensurePrincipal(user) {
			await unaryCall(runtime, "EnsurePrincipal", {
				subject: user.subject,
				email: user.email,
			});
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
		async listServices(user, projectId) {
			const response = await unaryCall(
				runtime,
				"ListServices",
				{ projectId },
				user,
			);
			return decodeListServicesResponse(response);
		},
		async inspectRepositorySource(user, input) {
			const response = await unaryCall(
				runtime,
				"InspectSource",
				{
					provider: input.provider,
					repositorySelector: input.repositorySelector,
				},
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
		async listServiceLogs(user, input) {
			const response = await unaryCall(
				runtime,
				"ListServiceLogs",
				encodeListServiceLogsRequest(input),
				user,
			);
			return decodeListServiceLogsResponse(response).lines.map(
				({ projectId: _projectId, ...line }) => line,
			);
		},
		async listServiceDeployments() {
			return [];
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
		async createDomainBinding(user, input) {
			const response = await unaryCall(
				runtime,
				"CreateDomainBinding",
				{
					projectId: input.projectId,
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
					projectId: input.projectId,
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
				{ projectId: input.projectId, hostname: input.hostname },
				user,
			);
		},
	};
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
