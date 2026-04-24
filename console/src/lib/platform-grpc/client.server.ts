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
	CreateDomainBindingRequest,
	CreateProjectRequest,
	CreateServiceRequest,
	DeleteDomainBindingRequest,
	EnsurePrincipalRequest,
	GetServiceRequest,
	GetServiceStatusRequest,
	InspectSourceRequest,
	ListDomainBindingsRequest,
	ListServiceDeploymentsRequest,
	ListServiceLogsRequest,
	ListServicesRequest,
	OpsClient,
	PlatformClient,
	PlatformMethod,
	PlatformRequestMap,
	PlatformRuntimeConfig,
	RawUnaryCallback,
	UpdateDomainBindingRequest,
	UpdateServiceRequest,
} from "#/lib/platform-grpc/types.server";

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
		metadata.set("x-platform-user-subject", user.subject);
		metadata.set("x-platform-user-email", user.email);
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
				case "EnsurePrincipal":
					client.EnsurePrincipal(
						request as EnsurePrincipalRequest,
						metadata,
						handleResponse,
					);
					return;
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
