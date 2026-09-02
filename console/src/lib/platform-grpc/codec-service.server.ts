import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardDeploymentRecord,
	DashboardDeploymentStage,
	DashboardDeploymentStatus,
	DashboardDomainBinding,
	DashboardEnvironment,
	DashboardIndexedServiceStatus,
	DashboardIndexedServices,
	DashboardRepositoryInspection,
	DashboardServiceRecord,
	DashboardServiceSpec,
	DashboardServiceStatus,
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
import {
	decodeBuildState,
	decodeDeploymentAction,
	decodeDeploymentCauseKind,
	decodeDeploymentStageState,
	decodeDeploymentState,
	decodeDomainOwnershipState,
	decodeProjectKind,
	decodeServiceLogType,
	decodeUnappliedChangeAction,
	encodeServiceLogType,
} from "./codec-enums.server";
import {
	readArray,
	readBoolean,
	readNumberArray,
	readNumberLikeValue,
	readOptionalDate,
	readOptionalNumberLike,
	readOptionalRecord,
	readOptionalString,
	readRecord,
	readRequiredNumber,
	readRequiredString,
} from "./codec-read.server";
import {
	decodeOptionalBuildRecipe,
	decodeServiceSourceSummary,
	decodeServiceSpec,
	encodeRuntimeSpec,
	encodeServiceSource,
} from "./codec-spec.server";

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
				placementRegion: input.spec.placementRegion,
				rollingStrategy: input.spec.rollingStrategy,
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
				placementRegion: input.spec.placementRegion,
				rollingStrategy: input.spec.rollingStrategy,
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
		rolloutState: readOptionalString(value, "rolloutState"),
		drainStartedAt: readOptionalDate(value, "drainStartedAt"),
		drainDeadline: readOptionalDate(value, "drainDeadline"),
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
