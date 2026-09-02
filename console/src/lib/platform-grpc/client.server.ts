import { dirname, resolve } from "node:path";
import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";
import googleProtoFiles from "google-proto-files";

import {
	type DashboardUser,
	PlatformGatewayError,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/dashboard/core/utils.server";
import type {
	ApplyDeploymentActionRequest,
	CreateDomainBindingRequest,
	CreateProjectRequest,
	CreateServiceRequest,
	DeleteDomainBindingRequest,
	DeleteServiceRequest,
	DiscardServiceChangesRequest,
	GenerateDomainBindingRequest,
	GetEnvironmentRequest,
	GetServiceRequest,
	GetServiceStatusRequest,
	InspectSourceRequest,
	LinkGitHubRepositoryRequest,
	ListDomainBindingsRequest,
	ListEnvironmentsRequest,
	ListServiceDeploymentsRequest,
	ListServiceLogsRequest,
	ListServicesRequest,
	OpsClient,
	OpsMethod,
	OpsRequestMap,
	PlatformClient,
	PlatformMethod,
	PlatformRequestMap,
	PlatformRuntimeConfig,
	RawUnaryCallback,
	ScaleServiceRequest,
	UpdateDomainBindingRequest,
	UpdateServiceRequest,
} from "#/lib/platform-grpc/types.server";
import { createPlatformUserAssertion } from "#/lib/platform-grpc/user-assertion.server";

let clientInstance: PlatformClient | undefined;
let opsClientInstance: OpsClient | undefined;

export async function unaryCall<M extends PlatformMethod>(
	runtime: PlatformRuntimeConfig,
	method: M,
	request: PlatformRequestMap[M],
	user?: DashboardUser,
): Promise<unknown> {
	const client = getPlatformClient(runtime);
	const metadata = new grpc.Metadata();
	if (user) {
		metadata.set(
			"x-platform-user-assertion",
			createPlatformUserAssertion({
				secret: runtime.userAssertionSecret,
				userId: user.id,
			}),
		);
	}

	try {
		return await new Promise<unknown>((resolve, reject) => {
			const handleResponse: RawUnaryCallback = (error, response) => {
				if (error) {
					reject(error);
					return;
				}
				resolve(response);
			};

			switch (method) {
				case "ListProjects":
					client.ListProjects(
						request as Record<string, never>,
						metadata,
						handleResponse,
					);
					return;
				case "CreateProject":
					client.CreateProject(
						request as CreateProjectRequest,
						metadata,
						handleResponse,
					);
					return;
				case "ListEnvironments":
					client.ListEnvironments(
						request as ListEnvironmentsRequest,
						metadata,
						handleResponse,
					);
					return;
				case "GetEnvironment":
					client.GetEnvironment(
						request as GetEnvironmentRequest,
						metadata,
						handleResponse,
					);
					return;
				case "CreateEnvironment":
				case "DuplicateEnvironment":
				case "RenameEnvironment":
				case "DeleteEnvironment":
				case "DeployEnvironment":
					client[method](request as never, metadata, handleResponse);
					return;
				case "LinkGitHubRepository":
					client.LinkGitHubRepository(
						request as LinkGitHubRepositoryRequest,
						metadata,
						handleResponse,
					);
					return;
				case "InspectSource":
					client.InspectSource(
						request as InspectSourceRequest,
						metadata,
						handleResponse,
					);
					return;
				case "ListServices":
					client.ListServices(
						request as ListServicesRequest,
						metadata,
						handleResponse,
					);
					return;
				case "CreateService":
					client.CreateService(
						request as CreateServiceRequest,
						metadata,
						handleResponse,
					);
					return;
				case "UpdateService":
					client.UpdateService(
						request as UpdateServiceRequest,
						metadata,
						handleResponse,
					);
					return;
				case "ApplyDeploymentAction":
					client.ApplyDeploymentAction(
						request as ApplyDeploymentActionRequest,
						metadata,
						handleResponse,
					);
					return;
				case "ScaleService":
					client.ScaleService(
						request as ScaleServiceRequest,
						metadata,
						handleResponse,
					);
					return;
				case "DiscardServiceChanges":
					client.DiscardServiceChanges(
						request as DiscardServiceChangesRequest,
						metadata,
						handleResponse,
					);
					return;
				case "DeleteService":
					client.DeleteService(
						request as DeleteServiceRequest,
						metadata,
						handleResponse,
					);
					return;
				case "GetService":
					client.GetService(
						request as GetServiceRequest,
						metadata,
						handleResponse,
					);
					return;
				case "GetServiceStatus":
					client.GetServiceStatus(
						request as GetServiceStatusRequest,
						metadata,
						handleResponse,
					);
					return;
				case "ListServiceLogs":
					client.ListServiceLogs(
						request as ListServiceLogsRequest,
						metadata,
						handleResponse,
					);
					return;
				case "ListServiceDeployments":
					client.ListServiceDeployments(
						request as ListServiceDeploymentsRequest,
						metadata,
						handleResponse,
					);
					return;
				case "ListDomainBindings":
					client.ListDomainBindings(
						request as ListDomainBindingsRequest,
						metadata,
						handleResponse,
					);
					return;
				case "GenerateDomainBinding":
					client.GenerateDomainBinding(
						request as GenerateDomainBindingRequest,
						metadata,
						handleResponse,
					);
					return;
				case "CreateDomainBinding":
					client.CreateDomainBinding(
						request as CreateDomainBindingRequest,
						metadata,
						handleResponse,
					);
					return;
				case "UpdateDomainBinding":
					client.UpdateDomainBinding(
						request as UpdateDomainBindingRequest,
						metadata,
						handleResponse,
					);
					return;
				case "DeleteDomainBinding":
					client.DeleteDomainBinding(
						request as DeleteDomainBindingRequest,
						metadata,
						handleResponse,
					);
					return;
				default:
					reject(new Error(`unsupported platform method: ${method}`));
					return;
			}
		});
	} catch (cause) {
		throw toPlatformGatewayError(method, cause);
	}
}

export async function opsUserCall<M extends OpsMethod>(
	runtime: PlatformRuntimeConfig,
	method: M,
	request: OpsRequestMap[M],
	user: DashboardUser,
): Promise<unknown> {
	const client = getOpsClient(runtime);
	const metadata = new grpc.Metadata();
	metadata.set(
		"x-platform-user-assertion",
		createPlatformUserAssertion({
			secret: runtime.userAssertionSecret,
			userId: user.id,
		}),
	);
	try {
		return await new Promise<unknown>((resolve, reject) => {
			const callback: RawUnaryCallback = (error, response) => {
				if (error) reject(error);
				else resolve(response);
			};
			switch (method) {
				case "ListFleet":
					client.ListFleet(
						request as Record<string, never>,
						metadata,
						callback,
					);
					return;
				case "CreateAgent":
					client.CreateAgent(
						request as OpsRequestMap["CreateAgent"],
						metadata,
						callback,
					);
					return;
				case "UpdateAgent":
					client.UpdateAgent(
						request as OpsRequestMap["UpdateAgent"],
						metadata,
						callback,
					);
					return;
				case "SetAgentLifecycle":
					client.SetAgentLifecycle(
						request as OpsRequestMap["SetAgentLifecycle"],
						metadata,
						callback,
					);
					return;
			}
		});
	} catch (cause) {
		throw toPlatformGatewayError(method, cause);
	}
}

function getPlatformClient(runtime: PlatformRuntimeConfig): PlatformClient {
	if (clientInstance) {
		return clientInstance;
	}
	const loaded = loadProtoDefinition();
	const credentials = createCredentials(runtime);
	clientInstance = new loaded.platform.v1.PlatformService(
		runtime.controlPlaneAddress,
		credentials,
		{
			"grpc.ssl_target_name_override": runtime.controlPlaneServerName,
			"grpc.default_authority": runtime.controlPlaneServerName,
		},
	) as unknown as PlatformClient;
	return clientInstance;
}

export function getOpsClient(runtime: PlatformRuntimeConfig): OpsClient {
	if (opsClientInstance) {
		return opsClientInstance;
	}
	const loaded = loadProtoDefinition();
	const credentials = createCredentials(runtime);
	opsClientInstance = new loaded.platform.v1.OpsService(
		runtime.controlPlaneAddress,
		credentials,
		{
			"grpc.ssl_target_name_override": runtime.controlPlaneServerName,
			"grpc.default_authority": runtime.controlPlaneServerName,
		},
	) as unknown as OpsClient;
	return opsClientInstance;
}

function loadProtoDefinition(): {
	platform: {
		v1: {
			PlatformService: grpc.ServiceClientConstructor;
			OpsService: grpc.ServiceClientConstructor;
		};
	};
} {
	const protoPath = resolve(process.cwd(), "../api/proto/platform.proto");
	const definition = protoLoader.loadSync(protoPath, {
		longs: String,
		enums: String,
		defaults: true,
		oneofs: true,
		includeDirs: [
			resolve(process.cwd(), "../api/proto"),
			dirname(googleProtoFiles.getProtoPath()),
		],
	});
	return grpc.loadPackageDefinition(definition) as {
		platform: {
			v1: {
				PlatformService: grpc.ServiceClientConstructor;
				OpsService: grpc.ServiceClientConstructor;
			};
		};
	};
}

function createCredentials(
	runtime: PlatformRuntimeConfig,
): grpc.ChannelCredentials {
	const credentials = grpc.credentials.createSsl(
		runtime.controlPlaneCA,
		runtime.controlPlaneKey,
		runtime.controlPlaneCert,
	);
	return grpc.credentials.combineChannelCredentials(
		credentials,
		grpc.credentials.createFromMetadataGenerator((_params, callback) => {
			callback(null, new grpc.Metadata());
		}),
	);
}

export function toPlatformGatewayError(
	operation: string,
	cause: unknown,
): PlatformGatewayError {
	if (cause instanceof PlatformGatewayError) {
		return cause;
	}
	return new PlatformGatewayError({
		operation,
		message: formatError(cause),
		cause,
		grpcCode: isGrpcError(cause) ? cause.code : undefined,
	});
}

function isGrpcError(error: unknown): error is grpc.ServiceError {
	return Boolean(
		error &&
			typeof error === "object" &&
			"code" in error &&
			typeof error.code === "number",
	);
}
