import { dirname, resolve } from "node:path";
import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";
import googleProtoFiles from "google-proto-files";

import {
	PlatformGatewayError,
	type DashboardAllocationStatus,
	type DashboardBuildRecipe,
	type DashboardBuildState,
	type DashboardBuildStatus,
	type DashboardDomainBinding,
	type DashboardProject,
	type DashboardRepositoryInspection,
	type DashboardResolvedSourceBinding,
	type DashboardServiceRecord,
	type DashboardServiceSourceSummary,
	type DashboardServiceStatus,
	type DashboardSourceSpec,
	type DashboardUser,
	type PlatformGateway,
	type RepositoryAccessState,
} from "#/lib/dashboard-core.server";

const defaultServiceContainerPort = 8080;

export interface PlatformRuntimeConfig {
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
}

interface EnsurePrincipalRequest {
	subject: string;
	email: string;
}

interface CreateProjectRequest {
	name: string;
}

interface InspectSourceRequest {
	provider: string;
	repositorySelector: string;
}

interface ListServicesRequest {
	projectId: string;
}

interface CreateServiceRequest {
	projectId: string;
	service: {
		name: string;
		spec: {
			runtime: {
				containerPort: number;
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

interface UpdateServiceRequest {
	projectId: string;
	serviceId: string;
	service: {
		spec: {
			runtime: {
				containerPort: number;
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

interface GetServiceRequest {
	projectId: string;
	serviceId: string;
}

interface GetServiceStatusRequest {
	projectId: string;
	serviceId: string;
}

interface ListDomainBindingsRequest {
	projectId: string;
	serviceId: string;
}

interface CreateDomainBindingRequest {
	projectId: string;
	binding: {
		hostname: string;
		serviceId: string;
	};
}

export interface IngestGitHubWebhookInput {
	deliveryId: string;
	eventType: string;
	signature256: string;
	payload: Uint8Array;
}

type PlatformMethod =
	| "EnsurePrincipal"
	| "ListProjects"
	| "CreateProject"
	| "InspectSource"
	| "ListServices"
	| "CreateService"
	| "UpdateService"
	| "GetService"
	| "GetServiceStatus"
	| "ListDomainBindings"
	| "CreateDomainBinding";

type PlatformRequestMap = {
	EnsurePrincipal: EnsurePrincipalRequest;
	ListProjects: Record<string, never>;
	CreateProject: CreateProjectRequest;
	InspectSource: InspectSourceRequest;
	ListServices: ListServicesRequest;
	CreateService: CreateServiceRequest;
	UpdateService: UpdateServiceRequest;
	GetService: GetServiceRequest;
	GetServiceStatus: GetServiceStatusRequest;
	ListDomainBindings: ListDomainBindingsRequest;
	CreateDomainBinding: CreateDomainBindingRequest;
};

type RawUnaryCallback = (
	error: grpc.ServiceError | null,
	response: unknown,
) => void;

type PlatformClient = grpc.Client & {
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
};

type OpsClient = grpc.Client & {
	IngestGitHubWebhook: (
		request: IngestGitHubWebhookInput,
		metadata: grpc.Metadata,
		callback: RawUnaryCallback,
	) => void;
};

interface PlatformProjectMessage {
	id: string;
	name: string;
	kind: DashboardProject["kind"];
	systemKey?: string;
}

interface ListProjectsResponseMessage {
	projects: Array<PlatformProjectMessage>;
}

let clientInstance: PlatformClient | undefined;
let opsClientInstance: OpsClient | undefined;

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
					},
				},
				user,
			);
			return decodeDomainBindingMessage(response);
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
				input,
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

export function decodeProjectMessage(raw: unknown): PlatformProjectMessage {
	const value = readRecord(raw, "project");
	return {
		id: readRequiredString(value, "id", "project"),
		name: readRequiredString(value, "name", "project"),
		kind: decodeProjectKind(value.kind),
		systemKey: readOptionalString(value, "systemKey"),
	};
}

export function decodeProjectKind(raw: unknown): DashboardProject["kind"] {
	switch (raw) {
		case "PROJECT_KIND_USER":
		case "user":
			return "user";
		case "PROJECT_KIND_MANAGED":
		case "managed":
			return "managed";
		default:
			throw new Error(`invalid project kind: ${String(raw)}`);
	}
}

export function decodeSourceAccessState(raw: unknown): RepositoryAccessState {
	switch (raw) {
		case "SOURCE_ACCESS_STATE_AVAILABLE":
		case "available":
			return "available";
		case "SOURCE_ACCESS_STATE_INSTALLATION_REQUIRED":
		case "installation_required":
			return "installation_required";
		case "SOURCE_ACCESS_STATE_ACCESS_REVOKED":
		case "access_revoked":
			return "access_revoked";
		case "SOURCE_ACCESS_STATE_REPOSITORY_DELETED":
		case "repository_deleted":
			return "repository_deleted";
		default:
			return "unspecified";
	}
}

export function decodeBuildState(raw: unknown): DashboardBuildState {
	switch (raw) {
		case "BUILD_STATE_QUEUED":
		case "queued":
			return "queued";
		case "BUILD_STATE_RUNNING":
		case "running":
			return "running";
		case "BUILD_STATE_SUCCEEDED":
		case "succeeded":
			return "succeeded";
		case "BUILD_STATE_FAILED":
		case "failed":
			return "failed";
		case "BUILD_STATE_SUPERSEDED":
		case "superseded":
			return "superseded";
		default:
			return "unspecified";
	}
}

function decodeListProjectsResponse(raw: unknown): ListProjectsResponseMessage {
	const value = readRecord(raw, "list projects response");
	const projects = readArray(value, "projects");
	return {
		projects: projects.map((project) => decodeProjectMessage(project)),
	};
}

function decodeInspectSourceResponse(
	raw: unknown,
): DashboardRepositoryInspection {
	const value = readRecord(raw, "inspect source response");
	return {
		accessState: decodeSourceAccessState(value.accessState),
		defaultBranch: readOptionalString(value, "defaultBranch") ?? "",
		dockerfileCandidates: readStringArray(value, "dockerfileCandidates"),
		recommendedBuildRecipe: decodeOptionalBuildRecipe(
			value.recommendedBuildRecipe,
		),
	};
}

export function encodeCreateServiceRequest(input: {
	projectId: string;
	name: string;
	source: DashboardSourceSpec;
}): CreateServiceRequest {
	return {
		projectId: input.projectId,
		service: {
			name: input.name,
			spec: {
				runtime: {
					containerPort:
						input.source.containerPort ?? defaultServiceContainerPort,
				},
				source: {
					sourceSpec: {
						provider: input.source.provider,
						repositorySelector: input.source.repositorySelector,
						trackedRef: input.source.trackedRef,
						buildRecipe: encodeBuildRecipe(input.source.buildRecipe),
					},
				},
			},
		},
	};
}

export function encodeUpdateServiceRequest(input: {
	projectId: string;
	serviceId: string;
	source: DashboardSourceSpec;
}): UpdateServiceRequest {
	return {
		projectId: input.projectId,
		serviceId: input.serviceId,
		service: {
			spec: {
				runtime: {
					containerPort:
						input.source.containerPort ?? defaultServiceContainerPort,
				},
				source: {
					sourceSpec: {
						provider: input.source.provider,
						repositorySelector: input.source.repositorySelector,
						trackedRef: input.source.trackedRef,
						buildRecipe: encodeBuildRecipe(input.source.buildRecipe),
					},
				},
			},
		},
	};
}

function decodeListServicesResponse(
	raw: unknown,
): Array<DashboardServiceRecord> {
	const value = readRecord(raw, "list services response");
	return readArray(value, "services").map((service) =>
		decodeServiceMessage(service),
	);
}

function decodeServiceMessage(raw: unknown): DashboardServiceRecord {
	const value = readRecord(raw, "service");
	return {
		id: readRequiredString(value, "id", "service"),
		projectId: readRequiredString(value, "projectId", "service"),
		name: readRequiredString(value, "name", "service"),
		spec: decodeServiceSpec(value.spec),
		sourceSummary: decodeServiceSourceSummary(value.sourceSummary),
		lastSuccessfulCommitSha:
			readOptionalString(value, "lastSuccessfulCommitSha") ?? undefined,
		resolvedImage: readOptionalString(value, "resolvedImage") ?? undefined,
		latestBuild: decodeBuildStatus(value.latestBuild),
	};
}

function decodeServiceSpec(raw: unknown): DashboardSourceSpec | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const runtime = readOptionalRecord(value.runtime);
	const source = readOptionalRecord(value.source);
	const sourceSpec = readOptionalRecord(source?.sourceSpec);
	if (!sourceSpec) {
		return undefined;
	}
	return {
		provider: readOptionalString(sourceSpec, "provider") ?? "",
		repositorySelector:
			readOptionalString(sourceSpec, "repositorySelector") ?? "",
		trackedRef: readOptionalString(sourceSpec, "trackedRef") ?? "",
		buildRecipe: decodeOptionalBuildRecipe(sourceSpec.buildRecipe),
		containerPort: readOptionalNumber(runtime, "containerPort"),
	};
}

function decodeServiceSourceSummary(
	raw: unknown,
): DashboardServiceSourceSummary | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const sourceState = readOptionalRecord(value.sourceState);
	if (!sourceState) {
		return undefined;
	}
	return {
		desiredSpec: decodeSourceSpec(sourceState.desiredSpec),
		resolvedBinding: decodeResolvedSourceBinding(sourceState.resolvedBinding),
	};
}

function decodeSourceSpec(raw: unknown): DashboardSourceSpec | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return {
		provider: readOptionalString(value, "provider") ?? "",
		repositorySelector: readOptionalString(value, "repositorySelector") ?? "",
		trackedRef: readOptionalString(value, "trackedRef") ?? "",
		buildRecipe: decodeOptionalBuildRecipe(value.buildRecipe),
		containerPort: readOptionalNumber(value, "containerPort"),
	};
}

function decodeResolvedSourceBinding(
	raw: unknown,
): DashboardResolvedSourceBinding | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return {
		repositorySelector: readOptionalString(value, "repositorySelector") ?? "",
		trackedRef: readOptionalString(value, "trackedRef") ?? "",
		accessState: decodeSourceAccessState(value.accessState),
		buildRecipe: decodeOptionalBuildRecipe(value.buildRecipe),
	};
}

function decodeBuildStatus(raw: unknown): DashboardBuildStatus | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const buildId = readOptionalString(value, "buildId");
	if (!buildId) {
		return undefined;
	}
	return {
		buildId,
		state: decodeBuildState(value.state),
		commitSha: readOptionalString(value, "commitSha") ?? "",
		imageDigest: readOptionalString(value, "imageDigest") ?? "",
		failureReason: readOptionalString(value, "failureReason") ?? "",
	};
}

function decodeServiceStatusMessage(raw: unknown): DashboardServiceStatus {
	const value = readRecord(raw, "service status");
	return {
		service: decodeServiceMessage(value.service),
		allocation: decodeAllocationStatus(value.allocation),
	};
}

function decodeAllocationStatus(
	raw: unknown,
): DashboardAllocationStatus | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return {
		phase: readOptionalString(value, "phase") ?? "",
		message: readOptionalString(value, "message") ?? "",
		endpointAddr: readOptionalString(value, "endpointAddr") ?? "",
		healthy: readBoolean(value, "healthy"),
	};
}

function decodeListDomainBindingsResponse(
	raw: unknown,
): Array<DashboardDomainBinding> {
	const value = readRecord(raw, "list domain bindings response");
	return readArray(value, "bindings").map((binding) =>
		decodeDomainBindingMessage(binding),
	);
}

function decodeDomainBindingMessage(raw: unknown): DashboardDomainBinding {
	const value = readRecord(raw, "domain binding");
	return {
		hostname: readRequiredString(value, "hostname", "domain binding"),
		projectId: readRequiredString(value, "projectId", "domain binding"),
		serviceId: readRequiredString(value, "serviceId", "domain binding"),
	};
}

function encodeBuildRecipe(
	recipe: DashboardBuildRecipe | undefined,
): CreateServiceRequest["service"]["spec"]["source"]["sourceSpec"]["buildRecipe"] {
	if (!recipe) {
		return undefined;
	}
	return {
		dockerfilePath: recipe.dockerfilePath,
		contextDir: recipe.contextDir,
	};
}

function decodeOptionalBuildRecipe(
	raw: unknown,
): DashboardBuildRecipe | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const dockerfilePath = readOptionalString(value, "dockerfilePath") ?? "";
	const contextDir = readOptionalString(value, "contextDir") ?? "";
	if (dockerfilePath === "" && contextDir === "") {
		return undefined;
	}
	return {
		dockerfilePath,
		contextDir,
	};
}

async function unaryCall<M extends PlatformMethod>(
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

function getOpsClient(runtime: PlatformRuntimeConfig): OpsClient {
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

function toPlatformGatewayError(
	operation: string,
	cause: unknown,
): PlatformGatewayError {
	if (cause instanceof PlatformGatewayError) {
		return cause;
	}
	return new PlatformGatewayError({
		operation,
		message: formatCause(cause),
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

function formatCause(cause: unknown): string {
	if (cause && typeof cause === "object" && "message" in cause) {
		return String(cause.message);
	}
	return "unknown error";
}

function readRecord(raw: unknown, context: string): Record<string, unknown> {
	if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
		throw new Error(`invalid ${context}: expected an object`);
	}
	return raw as Record<string, unknown>;
}

function readOptionalRecord(
	raw: unknown,
): Record<string, unknown> | undefined {
	if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
		return undefined;
	}
	return raw as Record<string, unknown>;
}

function readArray(
	value: Record<string, unknown>,
	key: string,
): Array<unknown> {
	const array = value[key];
	if (!Array.isArray(array)) {
		return [];
	}
	return array;
}

function readRequiredString(
	value: Record<string, unknown>,
	key: string,
	context: string,
): string {
	const candidate = value[key];
	if (typeof candidate !== "string" || candidate === "") {
		throw new Error(`invalid ${context}.${key}`);
	}
	return candidate;
}

function readOptionalString(
	value: Record<string, unknown> | undefined,
	key: string,
): string | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	return typeof candidate === "string" ? candidate : undefined;
}

function readOptionalNumber(
	value: Record<string, unknown> | undefined,
	key: string,
): number | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	return typeof candidate === "number" ? candidate : undefined;
}

function readBoolean(value: Record<string, unknown>, key: string): boolean {
	return value[key] === true;
}

function readStringArray(
	value: Record<string, unknown>,
	key: string,
): Array<string> {
	return readArray(value, key).filter(
		(entry): entry is string => typeof entry === "string",
	);
}
