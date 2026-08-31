import type {
	DashboardAllocationStatus,
	DashboardBuildRecipe,
	DashboardBuildState,
	DashboardBuildStatus,
	DashboardDeploymentAction,
	DashboardDeploymentCauseKind,
	DashboardDeploymentRecord,
	DashboardDeploymentStage,
	DashboardDeploymentStageState,
	DashboardDeploymentState,
	DashboardDeploymentStatus,
	DashboardDomainBinding,
	DashboardEnvironment,
	DashboardIndexedServiceStatus,
	DashboardIndexedServices,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardResolvedSourceBinding,
	DashboardRuntimePort,
	DashboardServiceLogType,
	DashboardServiceRecord,
	DashboardServiceSourceSummary,
	DashboardServiceSpec,
	DashboardServiceStatus,
	DashboardSourceSpec,
	DashboardUnappliedChangeAction,
	RepositoryAccessState,
} from "#/lib/dashboard/core/types.server";
import {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "#/lib/dashboard/core/types.server";
import type {
	CreateServiceRequest,
	IngestGitHubWebhookInput,
	IngestGitHubWebhookRequest,
	ListEnvironmentsResponseMessage,
	ListProjectsResponseMessage,
	ListServiceDeploymentsResponseMessage,
	ListServiceLogsRequest,
	ListServiceLogsResponseMessage,
	PlatformProjectMessage,
	ServiceLogLineMessage,
	UpdateServiceRequest,
} from "#/lib/platform-grpc/types.server";

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
		case "BUILD_STATE_CANCELLED":
		case "cancelled":
			return "cancelled";
		default:
			return "unspecified";
	}
}

export function decodeDeploymentStageState(
	raw: unknown,
): DashboardDeploymentStageState {
	switch (raw) {
		case "DEPLOYMENT_STAGE_STATE_PENDING":
		case "pending":
			return "pending";
		case "DEPLOYMENT_STAGE_STATE_RUNNING":
		case "running":
			return "running";
		case "DEPLOYMENT_STAGE_STATE_SUCCEEDED":
		case "succeeded":
			return "succeeded";
		case "DEPLOYMENT_STAGE_STATE_FAILED":
		case "failed":
			return "failed";
		case "DEPLOYMENT_STAGE_STATE_SKIPPED":
		case "skipped":
			return "skipped";
		default:
			return "unspecified";
	}
}

export function decodeServiceLogType(raw: unknown): DashboardServiceLogType {
	switch (raw) {
		case "SERVICE_LOG_TYPE_RUNTIME":
		case "runtime":
			return "runtime";
		case "SERVICE_LOG_TYPE_BUILD":
		case "build":
			return "build";
		case "SERVICE_LOG_TYPE_DEPLOY":
		case "deploy":
			return "deploy";
		case "SERVICE_LOG_TYPE_HTTP":
		case "http":
			return "http";
		case "SERVICE_LOG_TYPE_NETWORK":
		case "network":
			return "network";
		default:
			return "unspecified";
	}
}

export function encodeListServiceLogsRequest(
	input: ListServiceLogsRequest,
): ListServiceLogsRequest {
	return {
		serviceId: input.serviceId,
		allocationId: input.allocationId,
		limit: input.limit,
		logType: encodeServiceLogType(
			input.logType,
		) as ListServiceLogsRequest["logType"],
		buildId: input.buildId,
		search: input.search,
		startTime: input.startTime,
		endTime: input.endTime,
	};
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

export function decodeEnvironmentMessage(raw: unknown): DashboardEnvironment {
	const value = readRecord(raw, "environment");
	const kind = value.kind;
	if (kind !== "ENVIRONMENT_KIND_PERSISTENT" && kind !== "persistent") {
		throw new Error(`invalid environment kind: ${String(kind)}`);
	}
	return {
		id: readRequiredString(value, "id", "environment"),
		projectId: readRequiredString(value, "projectId", "environment"),
		name: readRequiredString(value, "name", "environment"),
		kind: "persistent",
		isProduction: readBoolean(value, "isProduction"),
		copiedFromEnvironmentId: readOptionalString(
			value,
			"copiedFromEnvironmentId",
		),
		createdAt: readOptionalDate(value, "createdAt"),
		updatedAt: readOptionalDate(value, "updatedAt"),
	};
}

export function decodeListEnvironmentsResponse(
	raw: unknown,
): ListEnvironmentsResponseMessage {
	const value = readRecord(raw, "list environments response");
	return {
		environments: readArray(value, "environments").map(
			decodeEnvironmentMessage,
		),
	};
}

export function decodeListServiceLogsResponse(
	raw: unknown,
): ListServiceLogsResponseMessage {
	const value = readRecord(raw, "list service logs response");
	return {
		lines: readArray(value, "lines").map((line) => decodeServiceLogLine(line)),
	};
}

export function decodeListServiceDeploymentsResponse(
	raw: unknown,
): ListServiceDeploymentsResponseMessage {
	const value = readRecord(raw, "list service deployments response");
	return {
		deployments: readArray(value, "deployments").map((deployment) =>
			decodeDeploymentRecord(deployment),
		),
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
		recommendedPorts: readNumberArray(value, "recommendedPorts"),
	};
}

export function encodeCreateServiceRequest(input: {
	environmentId: string;
	name: string;
	spec: DashboardServiceSpec;
}): CreateServiceRequest {
	return {
		environmentId: input.environmentId,
		service: {
			name: input.name,
			spec: {
				runtime: encodeRuntimeSpec(input.spec.runtime),
				source: encodeServiceSource(input.spec.source),
				desiredReplicaCount: input.spec.desiredReplicaCount,
			},
		},
	};
}

export function encodeUpdateServiceRequest(input: {
	serviceId: string;
	name?: string;
	spec: DashboardServiceSpec;
}): UpdateServiceRequest {
	return {
		serviceId: input.serviceId,
		service: {
			name: input.name,
			spec: {
				runtime: encodeRuntimeSpec(input.spec.runtime),
				source: encodeServiceSource(input.spec.source),
				desiredReplicaCount: input.spec.desiredReplicaCount,
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

export function decodeIndexedServicesResponse(
	raw: unknown,
): DashboardIndexedServices {
	const value = readRecord(raw, "indexed services response");
	const notModified = readBoolean(value, "notModified");
	return {
		index: readOptionalNumberLike(value, "index") ?? 0,
		notModified,
		services: notModified
			? undefined
			: readArray(value, "services").map((service) =>
					decodeServiceMessage(service),
				),
	};
}

export function decodeServiceMessage(raw: unknown): DashboardServiceRecord {
	const value = readRecord(raw, "service");
	return {
		id: readRequiredString(value, "id", "service"),
		environmentId: readRequiredString(value, "environmentId", "service"),
		name: readRequiredString(value, "name", "service"),
		specRevision: readOptionalNumberLike(value, "specRevision") ?? undefined,
		rolloutGeneration:
			readOptionalNumberLike(value, "rolloutGeneration") ?? undefined,
		createdAt: readOptionalDate(value, "createdAt"),
		updatedAt: readOptionalDate(value, "updatedAt"),
		internalHostname:
			readOptionalString(value, "internalHostname") ?? undefined,
		spec: decodeServiceSpec(value.spec),
		sourceSummary: decodeServiceSourceSummary(value.sourceSummary),
		lastSuccessfulCommitSha:
			readOptionalString(value, "lastSuccessfulCommitSha") ?? undefined,
		resolvedImage: readOptionalString(value, "resolvedImage") ?? undefined,
		latestBuild: decodeBuildStatus(value.latestBuild),
		latestDeployment: decodeDeploymentStatus(value.latestDeployment),
		pendingChanges: readBoolean(value, "pendingChanges"),
		unappliedChangeCount:
			readOptionalNumberLike(value, "unappliedChangeCount") ?? 0,
		unappliedChanges: readArray(value, "unappliedChanges").map(
			decodeUnappliedChange,
		),
		desiredReplicaCount:
			readOptionalNumberLike(value, "desiredReplicaCount") ?? 1,
		readyReplicaCount: readOptionalNumberLike(value, "readyReplicaCount") ?? 0,
		placementMessage:
			readOptionalString(value, "placementMessage") ?? undefined,
	};
}

function decodeUnappliedChange(raw: unknown) {
	const value = readRecord(raw, "unapplied change");
	return {
		id: readOptionalString(value, "id") ?? "",
		section: readOptionalString(value, "section") ?? "",
		field: readOptionalString(value, "field") ?? "",
		path: readOptionalString(value, "path") ?? "",
		action: decodeUnappliedChangeAction(value.action),
		currentValue: readOptionalString(value, "currentValue") ?? "",
		newValue: readOptionalString(value, "newValue") ?? "",
	};
}

function decodeUnappliedChangeAction(
	raw: unknown,
): DashboardUnappliedChangeAction {
	switch (raw) {
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD":
		case 1:
			return "add";
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE":
		case 2:
			return "update";
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE":
		case 3:
			return "remove";
		default:
			return "unspecified";
	}
}

function decodeServiceSpec(raw: unknown): DashboardServiceSpec | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const runtime = decodeRuntimeSpec(value.runtime);
	const source = decodeServiceSource(value.source);
	if (
		!source &&
		runtime.ports.length === 0 &&
		Object.keys(runtime.env).length === 0
	) {
		return undefined;
	}
	const desiredReplicaCount = readOptionalNumberLike(
		value,
		"desiredReplicaCount",
	);
	return {
		source,
		runtime,
		...(desiredReplicaCount === undefined ? {} : { desiredReplicaCount }),
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
		queuedAt: readOptionalDate(value, "queuedAt"),
		startedAt: readOptionalDate(value, "startedAt"),
		finishedAt: readOptionalDate(value, "finishedAt"),
		failureReason: readOptionalString(value, "failureReason") ?? "",
		commitMessage: readOptionalString(value, "commitMessage") ?? undefined,
		commitAuthor: readOptionalString(value, "commitAuthor") ?? undefined,
		stages: readArray(value, "stages").map((stage) =>
			decodeDeploymentStage(stage),
		),
	};
}

function decodeDeploymentStage(raw: unknown): DashboardDeploymentStage {
	const value = readRecord(raw, "deployment stage");
	return {
		key: readOptionalString(value, "key") ?? "",
		label: readOptionalString(value, "label") ?? "",
		detail: readOptionalString(value, "detail") ?? "",
		state: decodeDeploymentStageState(value.state),
		startedAt: readOptionalDate(value, "startedAt"),
		finishedAt: readOptionalDate(value, "finishedAt"),
	};
}

export function decodeServiceStatusMessage(
	raw: unknown,
): DashboardServiceStatus {
	const value = readRecord(raw, "service status");
	return {
		service: decodeServiceMessage(value.service),
		allocation: decodeAllocationStatus(value.allocation),
		allocations: readArray(value, "allocations")
			.map((item) => decodeAllocationStatus(item))
			.filter((item): item is DashboardAllocationStatus => Boolean(item)),
	};
}

export function decodeIndexedServiceStatusResponse(
	raw: unknown,
): DashboardIndexedServiceStatus {
	const value = readRecord(raw, "indexed service status response");
	const notModified = readBoolean(value, "notModified");
	return {
		index: readOptionalNumberLike(value, "index") ?? 0,
		notModified,
		status: notModified ? undefined : decodeServiceStatusMessage(value),
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
		allocationId: readOptionalString(value, "allocationId") ?? "",
		serviceId: readOptionalString(value, "serviceId") ?? "",
		agentId: readOptionalString(value, "agentId") ?? "",
		desiredSpecRevision:
			readOptionalNumberLike(value, "desiredSpecRevision") ?? 0,
		appliedSpecRevision:
			readOptionalNumberLike(value, "appliedSpecRevision") ?? 0,
		phase: readOptionalString(value, "phase") ?? "",
		message: readOptionalString(value, "message") ?? "",
		allocationIp: readOptionalString(value, "allocationIp") ?? "",
		healthy: readBoolean(value, "healthy"),
		updatedAt: readOptionalDate(value, "updatedAt"),
		desiredRolloutGeneration:
			readOptionalNumberLike(value, "desiredRolloutGeneration") ?? 0,
		appliedRolloutGeneration:
			readOptionalNumberLike(value, "appliedRolloutGeneration") ?? 0,
		healthyPorts: readNumberArray(value, "healthyPorts"),
		operatorRestartNonce: readOptionalNumberLike(value, "operatorRestartNonce"),
		restart: decodeRestartObservation(value.restart),
	};
}

function decodeRestartObservation(
	raw: unknown,
): DashboardAllocationStatus["restart"] {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return {
		restartCount: readOptionalNumberLike(value, "restartCount") ?? 0,
		crashLoop: readBoolean(value, "crashLoop"),
		lastCause: readOptionalString(value, "lastCause") ?? "",
		message: readOptionalString(value, "message") ?? "",
	};
}

function decodeDeploymentRecord(raw: unknown): DashboardDeploymentRecord {
	const value = readRecord(raw, "deployment record");
	const specRevision = readOptionalNumberLike(value, "specRevision") ?? 0;
	return {
		id: readRequiredString(value, "id", "deployment record"),
		rolloutGeneration: readOptionalNumberLike(value, "rolloutGeneration") ?? 0,
		specRevision: specRevision > 0 ? specRevision : undefined,
		createdAt: readOptionalDate(value, "createdAt"),
		build: decodeBuildStatus(value.build),
		isCurrent: readBoolean(value, "isCurrent"),
		status: decodeDeploymentStatus(value.status),
		stages: readArray(value, "stages").map((stage) =>
			decodeDeploymentStage(stage),
		),
		imageDigest: readOptionalString(value, "imageDigest"),
		actions: readArray(value, "actions").map((action) => {
			const actionValue = readRecord(action, "deployment action");
			return {
				id: readRequiredString(actionValue, "id", "deployment action"),
				action: decodeDeploymentAction(actionValue.action),
				targetDeploymentId:
					readOptionalString(actionValue, "targetDeploymentId") ?? "",
				resultDeploymentId: readOptionalString(
					actionValue,
					"resultDeploymentId",
				),
				allocationId: readOptionalString(actionValue, "allocationId"),
				requestedByUserId:
					readOptionalString(actionValue, "requestedByUserId") ?? "",
				createdAt: readOptionalDate(actionValue, "createdAt"),
			};
		}),
		variableVersions: decodeVariableVersions(value.variableVersions),
	};
}

function decodeDeploymentAction(raw: unknown): DashboardDeploymentAction {
	switch (raw) {
		case "DEPLOYMENT_ACTION_RESTART":
		case "restart":
			return "restart";
		case "DEPLOYMENT_ACTION_EXACT_REDEPLOY":
		case "exact_redeploy":
			return "exact_redeploy";
		case "DEPLOYMENT_ACTION_ROLLBACK":
		case "rollback":
			return "rollback";
		case "DEPLOYMENT_ACTION_CANCEL":
		case "cancel":
			return "cancel";
		case "DEPLOYMENT_ACTION_REMOVE":
		case "remove":
			return "remove";
		case "DEPLOYMENT_ACTION_RETRY":
		case "retry":
			return "retry";
		default:
			throw new Error(`invalid deployment action: ${String(raw)}`);
	}
}

function decodeVariableVersions(raw: unknown): Record<string, number> {
	const value = readOptionalRecord(raw);
	if (!value) return {};
	return Object.fromEntries(
		Object.entries(value).map(([key, version]) => [
			key,
			readNumberLikeValue(version, `variable version ${key}`),
		]),
	);
}

function readNumberLikeValue(raw: unknown, context: string): number {
	const value =
		typeof raw === "number"
			? raw
			: typeof raw === "string" && raw.trim() !== ""
				? Number(raw)
				: Number.NaN;
	if (!Number.isFinite(value)) {
		throw new Error(`invalid ${context}`);
	}
	return value;
}

function decodeDeploymentStatus(
	raw: unknown,
): DashboardDeploymentStatus | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return {
		deploymentId: readOptionalString(value, "deploymentId") ?? "",
		state: decodeDeploymentState(value.state),
		transitionedAt: readOptionalDate(value, "transitionedAt"),
		causeKind: decodeDeploymentCauseKind(value.causeKind),
		causeId: readOptionalString(value, "causeId") ?? "",
		reasonCode: readOptionalString(value, "reasonCode") ?? "",
		detail: readOptionalString(value, "detail") ?? "",
		specRevision: readOptionalNumberLike(value, "specRevision") ?? 0,
		imageDigest: readOptionalString(value, "imageDigest") ?? "",
		rolloutGeneration: readOptionalNumberLike(value, "rolloutGeneration") ?? 0,
	};
}

export function decodeDeploymentState(raw: unknown): DashboardDeploymentState {
	switch (raw) {
		case "DEPLOYMENT_STATE_STAGED":
		case "staged":
			return "staged";
		case "DEPLOYMENT_STATE_QUEUED_BUILD":
		case "queued_build":
			return "queued_build";
		case "DEPLOYMENT_STATE_BUILDING":
		case "building":
			return "building";
		case "DEPLOYMENT_STATE_SCHEDULING":
		case "scheduling":
			return "scheduling";
		case "DEPLOYMENT_STATE_IMAGE_PULL":
		case "image_pull":
			return "image_pull";
		case "DEPLOYMENT_STATE_STARTING":
		case "starting":
			return "starting";
		case "DEPLOYMENT_STATE_READINESS":
		case "readiness":
			return "readiness";
		case "DEPLOYMENT_STATE_ACTIVE":
		case "active":
			return "active";
		case "DEPLOYMENT_STATE_DRAINING":
		case "draining":
			return "draining";
		case "DEPLOYMENT_STATE_COMPLETED":
		case "completed":
			return "completed";
		case "DEPLOYMENT_STATE_FAILED":
		case "failed":
			return "failed";
		case "DEPLOYMENT_STATE_CANCELLED":
		case "cancelled":
			return "cancelled";
		case "DEPLOYMENT_STATE_CRASHED":
		case "crashed":
			return "crashed";
		case "DEPLOYMENT_STATE_REMOVED":
		case "removed":
			return "removed";
		case "DEPLOYMENT_STATE_SUPERSEDED":
		case "superseded":
			return "superseded";
		default:
			return "unspecified";
	}
}

export function decodeDeploymentCauseKind(
	raw: unknown,
): DashboardDeploymentCauseKind {
	switch (raw) {
		case "DEPLOYMENT_CAUSE_KIND_USER":
		case "user":
			return "user";
		case "DEPLOYMENT_CAUSE_KIND_SYSTEM":
		case "system":
			return "system";
		case "DEPLOYMENT_CAUSE_KIND_AGENT":
		case "agent":
			return "agent";
		case "DEPLOYMENT_CAUSE_KIND_BUILDER":
		case "builder":
			return "builder";
		case "DEPLOYMENT_CAUSE_KIND_WEBHOOK":
		case "webhook":
			return "webhook";
		default:
			return "unspecified";
	}
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
		serviceId: readRequiredString(value, "serviceId", "domain binding"),
		targetPort: readRequiredNumber(value, "targetPort", "domain binding"),
		platformGenerated: readBoolean(value, "platformGenerated"),
		ownershipState: decodeDomainOwnershipState(value.ownershipState),
		ownershipMessage: readOptionalString(value, "ownershipMessage"),
	};
}

function decodeDomainOwnershipState(
	raw: unknown,
): DashboardDomainBinding["ownershipState"] {
	switch (raw) {
		case "DOMAIN_OWNERSHIP_STATE_VERIFIED":
		case "verified":
			return "verified";
		case "DOMAIN_OWNERSHIP_STATE_UNVERIFIED":
		case "unverified":
			return "unverified";
		default:
			return "unspecified";
	}
}

function decodeServiceLogLine(raw: unknown): ServiceLogLineMessage {
	const value = readRecord(raw, "service log line");
	return {
		observedAt: readOptionalDate(value, "observedAt"),
		environmentId: readRequiredString(
			value,
			"environmentId",
			"service log line",
		),
		serviceId: readRequiredString(value, "serviceId", "service log line"),
		allocationId: readOptionalString(value, "allocationId") ?? "",
		agentId: readOptionalString(value, "agentId") ?? "",
		stream: readOptionalString(value, "stream") ?? "",
		rolloutGeneration: readOptionalNumberLike(value, "rolloutGeneration") ?? 0,
		sequence: readOptionalNumberLike(value, "sequence") ?? 0,
		line: readOptionalString(value, "line") ?? "",
		logType: decodeServiceLogType(value.logType),
		buildId: readOptionalString(value, "buildId") ?? undefined,
		stage: readOptionalString(value, "stage") ?? undefined,
	};
}

function encodeServiceLogType(
	logType: DashboardServiceLogType | undefined,
): string | undefined {
	switch (logType) {
		case "runtime":
			return "SERVICE_LOG_TYPE_RUNTIME";
		case "build":
			return "SERVICE_LOG_TYPE_BUILD";
		case "deploy":
			return "SERVICE_LOG_TYPE_DEPLOY";
		case "http":
			return "SERVICE_LOG_TYPE_HTTP";
		case "network":
			return "SERVICE_LOG_TYPE_NETWORK";
		case "unspecified":
			return "SERVICE_LOG_TYPE_UNSPECIFIED";
		default:
			return undefined;
	}
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

function encodeRuntimeSpec(
	runtime: DashboardServiceSpec["runtime"],
): CreateServiceRequest["service"]["spec"]["runtime"] {
	return {
		env: runtime.env ?? {},
		cpuMillis: runtime.cpuMillis,
		memoryMebibytes: runtime.memoryMebibytes,
		ports: (runtime.ports ?? []).map((port) => ({
			port: port.port,
			primary: port.primary,
		})),
		healthCheck: runtime.healthCheck
			? {
					type: "TYPE_HTTP",
					path: runtime.healthCheck.path,
					port: runtime.healthCheck.port,
					timeoutSeconds: runtime.healthCheck.timeoutSeconds,
				}
			: undefined,
		livenessCheck: runtime.livenessCheck
			? {
					type: "TYPE_HTTP",
					path: runtime.livenessCheck.path,
					port: runtime.livenessCheck.port,
					timeoutSeconds: runtime.livenessCheck.timeoutSeconds,
				}
			: undefined,
		restart: encodeRestartSpec(runtime.restart),
		volumeName: runtime.volumeName,
	};
}

function encodeRestartSpec(
	restart: DashboardServiceSpec["runtime"]["restart"],
): CreateServiceRequest["service"]["spec"]["runtime"]["restart"] {
	if (!restart) {
		return undefined;
	}
	const policy =
		restart.policy === "always"
			? "RESTART_POLICY_ALWAYS"
			: restart.policy === "never"
				? "RESTART_POLICY_NEVER"
				: "RESTART_POLICY_ON_FAILURE";
	return {
		policy,
		maxRestarts: restart.maxRestarts,
		windowSeconds: restart.windowSeconds,
		initialDelayMs: restart.initialDelayMs,
		maxDelayMs: restart.maxDelayMs,
		backoffMultiplier: restart.backoffMultiplier,
		jitter: restart.jitter,
		stableAfterSeconds: restart.stableAfterSeconds,
	};
}

function encodeServiceSource(
	source: DashboardSourceSpec | undefined,
): CreateServiceRequest["service"]["spec"]["source"] {
	return {
		sourceSpec: {
			provider: source?.provider ?? "",
			repositorySelector: source?.repositorySelector ?? "",
			trackedRef: source?.trackedRef ?? "",
			buildRecipe: encodeBuildRecipe(source?.buildRecipe),
		},
	};
}

function decodeRuntimeSpec(raw: unknown): DashboardServiceSpec["runtime"] {
	const value = readOptionalRecord(raw);
	return {
		env: readStringMap(value, "env"),
		cpuMillis: readPositiveResource(
			value,
			"cpuMillis",
			DEFAULT_SERVICE_CPU_MILLIS,
		),
		memoryMebibytes: readPositiveResource(
			value,
			"memoryMebibytes",
			DEFAULT_SERVICE_MEMORY_MEBIBYTES,
		),
		ports: readArray(value ?? {}, "ports")
			.map((item) => decodeRuntimePort(item))
			.filter((item): item is DashboardRuntimePort => item !== undefined),
		healthCheck: decodeHTTPHealthCheck(value?.healthCheck),
		livenessCheck: decodeHTTPHealthCheck(value?.livenessCheck),
		restart: decodeRestartSpec(value?.restart),
		volumeName: readOptionalString(value, "volumeName") ?? undefined,
	};
}

function decodeRestartSpec(
	raw: unknown,
): DashboardServiceSpec["runtime"]["restart"] {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const policyRaw = readOptionalString(value, "policy") ?? "";
	const policy =
		policyRaw === "RESTART_POLICY_ALWAYS" || policyRaw === "always"
			? "always"
			: policyRaw === "RESTART_POLICY_NEVER" || policyRaw === "never"
				? "never"
				: "on-failure";
	return {
		policy,
		maxRestarts: readOptionalNumber(value, "maxRestarts"),
		windowSeconds: readOptionalNumber(value, "windowSeconds"),
		initialDelayMs: readOptionalNumber(value, "initialDelayMs"),
		maxDelayMs: readOptionalNumber(value, "maxDelayMs"),
		backoffMultiplier: readOptionalNumber(value, "backoffMultiplier"),
		jitter: readOptionalNumber(value, "jitter"),
		stableAfterSeconds: readOptionalNumber(value, "stableAfterSeconds"),
	};
}

function decodeHTTPHealthCheck(
	raw: unknown,
): DashboardServiceSpec["runtime"]["healthCheck"] {
	const value = readOptionalRecord(raw);
	if (!value || readOptionalString(value, "type") !== "TYPE_HTTP") {
		return undefined;
	}
	const path = readOptionalString(value, "path") ?? "";
	if (!path) {
		return undefined;
	}
	return {
		path,
		port: readOptionalNumber(value, "port") || undefined,
		timeoutSeconds: readOptionalNumber(value, "timeoutSeconds") || undefined,
	};
}

function readPositiveResource(
	value: Record<string, unknown> | undefined,
	key: string,
	fallback: number,
): number {
	const resource = readOptionalNumber(value, key);
	return resource !== undefined && resource > 0 ? resource : fallback;
}

function decodeRuntimePort(raw: unknown): DashboardRuntimePort | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const port = readOptionalNumber(value, "port");
	if (!port || !Number.isInteger(port) || port < 1 || port > 65535) {
		return undefined;
	}
	return {
		port,
		primary: readBoolean(value, "primary"),
	};
}

function decodeServiceSource(raw: unknown): DashboardSourceSpec | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return decodeSourceSpec(value.sourceSpec);
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

function readOptionalNumberLike(
	value: Record<string, unknown> | undefined,
	key: string,
): number | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	if (typeof candidate === "number") {
		return Number.isFinite(candidate) ? candidate : undefined;
	}
	if (typeof candidate === "string" && candidate.trim() !== "") {
		const parsed = Number(candidate);
		return Number.isFinite(parsed) ? parsed : undefined;
	}
	return undefined;
}

function readRequiredNumber(
	value: Record<string, unknown>,
	key: string,
	context: string,
): number {
	const candidate = readOptionalNumber(value, key);
	if (candidate === undefined) {
		throw new Error(`invalid ${context}.${key}`);
	}
	return candidate;
}

function readBoolean(value: Record<string, unknown>, key: string): boolean {
	return value[key] === true;
}

function readOptionalDate(
	value: Record<string, unknown> | undefined,
	key: string,
): Date | undefined {
	if (!value) {
		return undefined;
	}
	const candidate = value[key];
	if (candidate instanceof Date) {
		return candidate;
	}
	if (typeof candidate === "string" || typeof candidate === "number") {
		const parsed = new Date(candidate);
		return Number.isNaN(parsed.getTime()) ? undefined : parsed;
	}
	if (candidate && typeof candidate === "object") {
		const record = candidate as Record<string, unknown>;
		const seconds = readOptionalNumberLike(record, "seconds");
		const nanos = readOptionalNumberLike(record, "nanos") ?? 0;
		if (seconds !== undefined) {
			return new Date(seconds * 1000 + nanos / 1_000_000);
		}
		const toDate = (candidate as { toDate?: () => Date }).toDate;
		if (typeof toDate === "function") {
			return toDate.call(candidate);
		}
	}
	return undefined;
}

function readStringArray(
	value: Record<string, unknown>,
	key: string,
): Array<string> {
	return readArray(value, key).filter(
		(entry): entry is string => typeof entry === "string",
	);
}

function readStringMap(
	value: Record<string, unknown> | undefined,
	key: string,
): Record<string, string> {
	const record = readOptionalRecord(value?.[key]);
	if (!record) {
		return {};
	}
	const out: Record<string, string> = {};
	for (const [entryKey, entryValue] of Object.entries(record)) {
		if (typeof entryValue === "string") {
			out[entryKey] = entryValue;
		}
	}
	return out;
}

function readNumberArray(
	value: Record<string, unknown>,
	key: string,
): Array<number> {
	return readArray(value, key).filter(
		(entry): entry is number =>
			typeof entry === "number" && Number.isFinite(entry),
	);
}
