import type * as grpc from "@grpc/grpc-js";

import type {
	DashboardProject,
	DashboardServiceLogType,
} from "#/lib/dashboard/core/types.server";

export interface PlatformRuntimeConfig {
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
}

export interface EnsurePrincipalRequest {
	subject: string;
	email: string;
}

export interface CreateProjectRequest {
	name: string;
}

export interface InspectSourceRequest {
	provider: string;
	repositorySelector: string;
}

export interface ListServicesRequest {
	projectId: string;
}

export interface CreateServiceRequest {
	projectId: string;
	service: {
		name: string;
		spec: {
			runtime: {
				ports: Array<{
					port: number;
					primary: boolean;
				}>;
			};
			source: {
				sourceSpec: {
					provider: string;
					repositorySelector: string;
					trackedRef: string;
					buildRecipe?: {
						dockerfilePath: string;
						contextDir: string;
					};
				};
			};
		};
	};
}

export interface UpdateServiceRequest {
	projectId: string;
	serviceId: string;
	service: {
		name?: string;
		spec: {
			runtime: {
				ports: Array<{
					port: number;
					primary: boolean;
				}>;
			};
			source: {
				sourceSpec: {
					provider: string;
					repositorySelector: string;
					trackedRef: string;
					buildRecipe?: {
						dockerfilePath: string;
						contextDir: string;
					};
				};
			};
		};
	};
}

export interface GetServiceRequest {
	projectId: string;
	serviceId: string;
}

export interface GetServiceStatusRequest {
	projectId: string;
	serviceId: string;
}

export interface ListServiceLogsRequest {
	projectId: string;
	serviceId: string;
	allocationId?: string;
	limit?: number;
	logType?: DashboardServiceLogType;
	buildId?: string;
	search?: string;
	startTime?: Date;
	endTime?: Date;
}

export interface ListDomainBindingsRequest {
	projectId: string;
	serviceId: string;
}

export interface CreateDomainBindingRequest {
	projectId: string;
	binding: {
		hostname: string;
		serviceId: string;
		targetPort: number;
	};
}

export interface UpdateDomainBindingRequest {
	projectId: string;
	hostname: string;
	binding: {
		serviceId: string;
		targetPort: number;
	};
}

export interface DeleteDomainBindingRequest {
	projectId: string;
	hostname: string;
}

export interface IngestGitHubWebhookInput {
	deliveryId: string;
	eventType: string;
	signature256: string;
	payload: Uint8Array;
}

export interface IngestGitHubWebhookRequest {
	deliveryId: string;
	eventType: string;
	signature_256: string;
	payload: Uint8Array;
}

export type PlatformMethod =
	| "EnsurePrincipal"
	| "ListProjects"
	| "CreateProject"
	| "InspectSource"
	| "ListServices"
	| "CreateService"
	| "UpdateService"
	| "GetService"
	| "GetServiceStatus"
	| "ListServiceLogs"
	| "ListDomainBindings"
	| "CreateDomainBinding"
	| "UpdateDomainBinding"
	| "DeleteDomainBinding";

export type PlatformRequestMap = {
	EnsurePrincipal: EnsurePrincipalRequest;
	ListProjects: Record<string, never>;
	CreateProject: CreateProjectRequest;
	InspectSource: InspectSourceRequest;
	ListServices: ListServicesRequest;
	CreateService: CreateServiceRequest;
	UpdateService: UpdateServiceRequest;
	GetService: GetServiceRequest;
	GetServiceStatus: GetServiceStatusRequest;
	ListServiceLogs: ListServiceLogsRequest;
	ListDomainBindings: ListDomainBindingsRequest;
	CreateDomainBinding: CreateDomainBindingRequest;
	UpdateDomainBinding: UpdateDomainBindingRequest;
	DeleteDomainBinding: DeleteDomainBindingRequest;
};

export type RawUnaryCallback = (
	error: grpc.ServiceError | null,
	response: unknown,
) => void;

export type PlatformClient = grpc.Client & {
	EnsurePrincipal: (
		request: EnsurePrincipalRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	ListProjects: (
		request: Record<string, never>,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	CreateProject: (
		request: CreateProjectRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	InspectSource: (
		request: InspectSourceRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	ListServices: (
		request: ListServicesRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	CreateService: (
		request: CreateServiceRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	UpdateService: (
		request: UpdateServiceRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	GetService: (
		request: GetServiceRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	GetServiceStatus: (
		request: GetServiceStatusRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	ListServiceLogs: (
		request: ListServiceLogsRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	ListDomainBindings: (
		request: ListDomainBindingsRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	CreateDomainBinding: (
		request: CreateDomainBindingRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	UpdateDomainBinding: (
		request: UpdateDomainBindingRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	DeleteDomainBinding: (
		request: DeleteDomainBindingRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
};

export type OpsClient = grpc.Client & {
	IngestGitHubWebhook: (
		request: IngestGitHubWebhookRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
};

export interface PlatformProjectMessage {
	id: string;
	name: string;
	kind: DashboardProject["kind"];
	systemKey?: string;
}

export interface ListProjectsResponseMessage {
	projects: Array<PlatformProjectMessage>;
}

export interface ServiceLogLineMessage {
	observedAt?: Date;
	projectId: string;
	serviceId: string;
	allocationId: string;
	agentId: string;
	stream: string;
	rolloutGeneration: number;
	sequence: number;
	line: string;
	logType?: DashboardServiceLogType;
	buildId?: string;
	stage?: string;
}

export interface ListServiceLogsResponseMessage {
	lines: Array<ServiceLogLineMessage>;
}
