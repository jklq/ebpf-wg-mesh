import type {
	DashboardAllocationStatus,
	DashboardBuildRecipe,
	DashboardBuildState,
	DashboardBuildStatus,
	DashboardDomainBinding,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardResolvedSourceBinding,
	DashboardServiceRecord,
	DashboardServiceSourceSummary,
	DashboardServiceStatus,
	DashboardSourceSpec,
	RepositoryAccessState,
} from "#/lib/dashboard/core/types.server";
import type {
	CreateServiceRequest,
	IngestGitHubWebhookInput,
	IngestGitHubWebhookRequest,
	ListProjectsResponseMessage,
	PlatformProjectMessage,
	UpdateServiceRequest,
} from "#/lib/platform-grpc/types.server";

const defaultServiceContainerPort = 8080;

export function encodeIngestGitHubWebhookRequest(
	input: IngestGitHubWebhookInput,
): IngestGitHubWebhookRequest {
	return {
		deliveryId: input.deliveryId,
		eventType: input.eventType,
		signature_256: input.signature256,
		payload: input.payload,
	};
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

export function decodeListProjectsResponse(
	raw: unknown,
): ListProjectsResponseMessage {
	const value = readRecord(raw, "list projects response");
	const projects = readArray(value, "projects");
	return {
		projects: projects.map((project) => decodeProjectMessage(project)),
	};
}

export function decodeInspectSourceResponse(
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

export function decodeListServicesResponse(
	raw: unknown,
): Array<DashboardServiceRecord> {
	const value = readRecord(raw, "list services response");
	return readArray(value, "services").map((service) =>
		decodeServiceMessage(service),
	);
}

export function decodeServiceMessage(raw: unknown): DashboardServiceRecord {
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

export function decodeServiceStatusMessage(
	raw: unknown,
): DashboardServiceStatus {
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

export function decodeListDomainBindingsResponse(
	raw: unknown,
): Array<DashboardDomainBinding> {
	const value = readRecord(raw, "list domain bindings response");
	return readArray(value, "bindings").map((binding) =>
		decodeDomainBindingMessage(binding),
	);
}

export function decodeDomainBindingMessage(
	raw: unknown,
): DashboardDomainBinding {
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

function readRecord(raw: unknown, context: string): Record<string, unknown> {
	if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
		throw new Error(`invalid ${context}: expected an object`);
	}
	return raw as Record<string, unknown>;
}

function readOptionalRecord(raw: unknown): Record<string, unknown> | undefined {
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
