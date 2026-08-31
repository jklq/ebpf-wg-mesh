import type * as grpc from "@grpc/grpc-js";

import type {
	DashboardDeploymentRecord,
	DashboardEnvironment,
	DashboardProject,
	DashboardServiceLogType,
} from "#/lib/dashboard/core/types.server";

export interface PlatformRuntimeConfig {
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
	userAssertionSecret: string;
}

export interface CreateProjectRequest {
	name: string;
}

export interface InspectSourceRequest {
	projectId: string;
	provider: string;
	repositorySelector: string;
	githubUserAccessToken: string;
}

export interface LinkGitHubRepositoryRequest {
	projectId: string;
	repositorySelector: string;
	githubUserAccessToken: string;
}

export interface ListServicesRequest {
	environmentId: string;
	waitIndex?: number;
	waitTimeoutSeconds?: number;
}

export interface ListEnvironmentsRequest {
	projectId: string;
}
export interface GetEnvironmentRequest {
	environmentId: string;
}
export interface CreateEnvironmentRequest {
	projectId: string;
	name: string;
}
export interface DuplicateEnvironmentRequest {
	sourceEnvironmentId: string;
	name: string;
	copyVariables: boolean;
}
export interface RenameEnvironmentRequest {
	environmentId: string;
	name: string;
}
export interface DeleteEnvironmentRequest {
	environmentId: string;
}
export interface DeployEnvironmentRequest {
	environmentId: string;
}

export interface CreateServiceRequest {
	environmentId: string;
	service: {
		name: string;
		spec: {
			runtime: {
				env: Record<string, string>;
				cpuMillis: number;
				memoryMebibytes: number;
				ports: Array<{
					port: number;
					primary: boolean;
				}>;
				healthCheck?: {
					type: "TYPE_HTTP";
					path: string;
					port?: number;
					timeoutSeconds?: number;
				};
				livenessCheck?: {
					type: "TYPE_HTTP";
					path: string;
					port?: number;
					timeoutSeconds?: number;
				};
				restart?: {
					policy: string;
					maxRestarts?: number;
					windowSeconds?: number;
					initialDelayMs?: number;
					maxDelayMs?: number;
					backoffMultiplier?: number;
					jitter?: number;
					stableAfterSeconds?: number;
				};
				volumeName?: string;
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
			desiredReplicaCount?: number;
		};
	};
}

export interface UpdateServiceRequest {
	serviceId: string;
	service: {
		name?: string;
		spec: {
			runtime: {
				env: Record<string, string>;
				cpuMillis: number;
				memoryMebibytes: number;
				ports: Array<{
					port: number;
					primary: boolean;
				}>;
				healthCheck?: {
					type: "TYPE_HTTP";
					path: string;
					port?: number;
					timeoutSeconds?: number;
				};
				livenessCheck?: {
					type: "TYPE_HTTP";
					path: string;
					port?: number;
					timeoutSeconds?: number;
				};
				restart?: {
					policy: string;
					maxRestarts?: number;
					windowSeconds?: number;
					initialDelayMs?: number;
					maxDelayMs?: number;
					backoffMultiplier?: number;
					jitter?: number;
					stableAfterSeconds?: number;
				};
				volumeName?: string;
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
			desiredReplicaCount?: number;
		};
	};
}

export interface GetServiceRequest {
	serviceId: string;
}

export interface GetServiceStatusRequest {
	serviceId: string;
	waitIndex?: number;
	waitTimeoutSeconds?: number;
}

export interface RedeployServiceRequest {
	serviceId: string;
}

export interface ScaleServiceRequest {
	serviceId: string;
	desiredReplicaCount: number;
}

export interface RestartServiceRequest {
	serviceId: string;
}

export interface DeleteServiceRequest {
	serviceId: string;
}

export interface DiscardServiceChangesRequest {
	serviceId: string;
	changeIds?: Array<string>;
	discardAll?: boolean;
}

export interface ListServiceLogsRequest {
	serviceId: string;
	allocationId?: string;
	limit?: number;
	logType?: DashboardServiceLogType;
	buildId?: string;
	search?: string;
	startTime?: Date;
	endTime?: Date;
}

export interface ListServiceDeploymentsRequest {
	serviceId: string;
	limit?: number;
}

export interface ListDomainBindingsRequest {
	serviceId: string;
}

export interface CreateDomainBindingRequest {
	binding: {
		hostname: string;
		serviceId: string;
		targetPort: number;
	};
}

export interface GenerateDomainBindingRequest {
	serviceId: string;
	targetPort: number;
}

export interface UpdateDomainBindingRequest {
	hostname: string;
	binding: {
		serviceId: string;
		targetPort: number;
	};
}

export interface DeleteDomainBindingRequest {
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
	| "ListProjects"
	| "CreateProject"
	| "ListEnvironments"
	| "GetEnvironment"
	| "CreateEnvironment"
	| "DuplicateEnvironment"
	| "RenameEnvironment"
	| "DeleteEnvironment"
	| "DeployEnvironment"
	| "LinkGitHubRepository"
	| "InspectSource"
	| "ListServices"
	| "CreateService"
	| "UpdateService"
	| "RedeployService"
	| "ScaleService"
	| "RestartService"
	| "DiscardServiceChanges"
	| "DeleteService"
	| "GetService"
	| "GetServiceStatus"
	| "ListServiceLogs"
	| "ListServiceDeployments"
	| "ListDomainBindings"
	| "GenerateDomainBinding"
	| "CreateDomainBinding"
	| "UpdateDomainBinding"
	| "DeleteDomainBinding";

export type PlatformRequestMap = {
	ListProjects: Record<string, never>;
	CreateProject: CreateProjectRequest;
	ListEnvironments: ListEnvironmentsRequest;
	GetEnvironment: GetEnvironmentRequest;
	CreateEnvironment: CreateEnvironmentRequest;
	DuplicateEnvironment: DuplicateEnvironmentRequest;
	RenameEnvironment: RenameEnvironmentRequest;
	DeleteEnvironment: DeleteEnvironmentRequest;
	DeployEnvironment: DeployEnvironmentRequest;
	LinkGitHubRepository: LinkGitHubRepositoryRequest;
	InspectSource: InspectSourceRequest;
	ListServices: ListServicesRequest;
	CreateService: CreateServiceRequest;
	UpdateService: UpdateServiceRequest;
	RedeployService: RedeployServiceRequest;
	ScaleService: ScaleServiceRequest;
	RestartService: RestartServiceRequest;
	DiscardServiceChanges: DiscardServiceChangesRequest;
	DeleteService: DeleteServiceRequest;
	GetService: GetServiceRequest;
	GetServiceStatus: GetServiceStatusRequest;
	ListServiceLogs: ListServiceLogsRequest;
	ListServiceDeployments: ListServiceDeploymentsRequest;
	ListDomainBindings: ListDomainBindingsRequest;
	GenerateDomainBinding: GenerateDomainBindingRequest;
	CreateDomainBinding: CreateDomainBindingRequest;
	UpdateDomainBinding: UpdateDomainBindingRequest;
	DeleteDomainBinding: DeleteDomainBindingRequest;
};

export type RawUnaryCallback = (
	error: grpc.ServiceError | null,
	response: unknown,
) => void;

export type PlatformClient = grpc.Client & {
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
	ListEnvironments: (
		request: ListEnvironmentsRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	GetEnvironment: (
		request: GetEnvironmentRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	CreateEnvironment: (
		request: CreateEnvironmentRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	DuplicateEnvironment: (
		request: DuplicateEnvironmentRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	RenameEnvironment: (
		request: RenameEnvironmentRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	DeleteEnvironment: (
		request: DeleteEnvironmentRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	DeployEnvironment: (
		request: DeployEnvironmentRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	LinkGitHubRepository: (
		request: LinkGitHubRepositoryRequest,
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
	RedeployService: (
		request: RedeployServiceRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	ScaleService: (
		request: ScaleServiceRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	RestartService: (
		request: RestartServiceRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	DiscardServiceChanges: (
		request: DiscardServiceChangesRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	DeleteService: (
		request: DeleteServiceRequest,
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
	ListServiceDeployments: (
		request: ListServiceDeploymentsRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	ListDomainBindings: (
		request: ListDomainBindingsRequest,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
	GenerateDomainBinding: (
		request: GenerateDomainBindingRequest,
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

export interface PlatformEnvironmentMessage extends DashboardEnvironment {}
export interface ListEnvironmentsResponseMessage {
	environments: Array<PlatformEnvironmentMessage>;
}

export interface ServiceLogLineMessage {
	observedAt?: Date;
	environmentId: string;
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

export interface ListServiceDeploymentsResponseMessage {
	deployments: Array<DashboardDeploymentRecord>;
}
