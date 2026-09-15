import { create, type DescEnum, type DescEnumValue } from "@bufbuild/protobuf";
import {
	EmptySchema,
	type Timestamp,
	timestampDate,
	timestampFromDate,
} from "@bufbuild/protobuf/wkt";
import {
	type DashboardAgentEnrollment,
	type DashboardAgentLifecycleState,
	type DashboardAllocationStatus,
	type DashboardBuildRecipe,
	type DashboardBuildState,
	type DashboardBuildStatus,
	type DashboardDeploymentAction,
	type DashboardDeploymentActionRecord,
	type DashboardDeploymentCauseKind,
	type DashboardDeploymentRecord,
	type DashboardDeploymentStage,
	type DashboardDeploymentStageState,
	type DashboardDeploymentState,
	type DashboardDeploymentStatus,
	type DashboardDomainBinding,
	type DashboardDomainOwnershipState,
	type DashboardEnvironment,
	type DashboardFleet,
	type DashboardFleetAgent,
	type DashboardHTTPHealthCheck,
	type DashboardIndexedServiceStatus,
	type DashboardIndexedServices,
	type DashboardProject,
	type DashboardRepositoryInspection,
	type DashboardResolvedSourceBinding,
	type DashboardRestartSpec,
	type DashboardRollingStrategy,
	type DashboardRuntimePort,
	type DashboardRuntimeSpec,
	type DashboardServiceLogLine,
	type DashboardServiceLogType,
	type DashboardServiceRecord,
	type DashboardServiceSourceSummary,
	type DashboardServiceSpec,
	type DashboardServiceStatus,
	type DashboardSourceRevision,
	type DashboardSourceSpec,
	type DashboardUnappliedChange,
	type DashboardUnappliedChangeAction,
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "#/lib/dashboard/core/types.server";
import {
	type Agent,
	type AgentEnrollment,
	AgentLifecycleStateSchema,
	type AllocationStatus,
	ApplyDeploymentActionRequestSchema,
	type BuildRecipe,
	BuildStateSchema,
	type BuildStatus,
	type CreateAgentRequest,
	CreateAgentRequestSchema,
	CreateDomainBindingRequestSchema,
	CreateEnvironmentRequestSchema,
	CreateProjectRequestSchema,
	CreateServiceRequestSchema,
	DeleteDomainBindingRequestSchema,
	DeleteEnvironmentRequestSchema,
	DeleteServiceRequestSchema,
	type DeploymentActionRecord,
	DeploymentActionSchema,
	DeploymentCauseKindSchema,
	type DeploymentRecord,
	type DeploymentStage,
	DeploymentStageStateSchema,
	DeploymentStateSchema,
	type DeploymentStatus,
	DiscardServiceChangesRequestSchema,
	type DomainBinding,
	DomainOwnershipStateSchema,
	DuplicateEnvironmentRequestSchema,
	type Environment,
	type Fleet,
	GenerateDomainBindingRequestSchema,
	GetEnvironmentRequestSchema,
	GetServiceRequestSchema,
	type HealthCheck,
	HealthCheck_Type,
	type IngestGitHubWebhookRequest,
	IngestGitHubWebhookRequestSchema,
	InspectSourceRequestSchema,
	LinkGitHubRepositoryRequestSchema,
	ListDomainBindingsRequestSchema,
	ListEnvironmentsRequestSchema,
	ListServiceDeploymentsRequestSchema,
	type ListServiceDeploymentsResponse,
	type ListServiceLogsRequest,
	ListServiceLogsRequestSchema,
	type ListServiceLogsResponse,
	type ListServicesResponse,
	type Project,
	ProjectKind,
	ProjectKindSchema,
	ReleaseEnvironmentRequestSchema,
	RenameEnvironmentRequestSchema,
	type ResolvedSourceBinding,
	RestartCauseSchema,
	type RestartObservation,
	RestartPolicySchema,
	ScaleServiceRequestSchema,
	type Service,
	type ServiceLogLine,
	ServiceLogTypeSchema,
	type ServiceRestart,
	type ServiceRuntime,
	type ServiceRuntimePort,
	type ServiceSourceSpec,
	type ServiceSourceSummary,
	type ServiceSpec,
	type ServiceStatus,
	type ServiceUnappliedChange,
	ServiceUnappliedChangeActionSchema,
	type SetAgentLifecycleRequest,
	SetAgentLifecycleRequestSchema,
	SourceAccessStateSchema,
	type SourceRevision,
	type SourceStateSummary,
	UpdateAgentRequestSchema,
	UpdateDomainBindingRequestSchema,
	UpdateEnvironmentAutoDeployRequestSchema,
	type UpdateServiceRequest,
	UpdateServiceRequestSchema,
} from "#/lib/platform-gen/platform_pb";

function enumValueByNumber(
	desc: DescEnum,
	value: number,
): DescEnumValue | undefined {
	return desc.values.find((entry) => entry.number === value);
}

function enumName(desc: DescEnum, value: number): string {
	return enumValueByNumber(desc, value)?.name ?? desc.values[0].name;
}

function enumNumber(desc: DescEnum, name: string): number {
	return desc.values.find((entry) => entry.name === name)?.number ?? 0;
}

/** int64/uint64 fields are bigint on the wire and never lossy numbers. */
function safeNumber(value: bigint | number | undefined, fallback = 0): number {
	if (value === undefined) {
		return fallback;
	}
	const converted = typeof value === "bigint" ? Number(value) : value;
	if (!Number.isSafeInteger(converted)) {
		throw new Error(
			`protobuf int64 value is beyond the safe integer range: ${value}`,
		);
	}
	return converted;
}

function requireString(value: string, context: string): string {
	if (value === "") {
		throw new Error(`invalid ${context}: expected a non-empty string`);
	}
	return value;
}

function optionalDate(value: Timestamp | undefined): Date | undefined {
	return value ? timestampDate(value) : undefined;
}

export function toProject(project: Project): DashboardProject {
	return {
		id: requireString(project.id, "project.id"),
		name: requireString(project.name, "project.name"),
		kind:
			project.kind === ProjectKind.USER
				? "PROJECT_KIND_USER"
				: project.kind === ProjectKind.MANAGED
					? "PROJECT_KIND_MANAGED"
					: (() => {
							throw new Error(
								`invalid project kind: ${enumName(ProjectKindSchema, project.kind)}`,
							);
						})(),
		systemKey: project.systemKey || undefined,
	};
}

export function toProjects(projects: Project[]): DashboardProject[] {
	return projects.map(toProject);
}

export function toEnvironment(environment: Environment): DashboardEnvironment {
	if (environment.kind !== 1) {
		throw new Error(`invalid environment kind: ${environment.kind}`);
	}
	return {
		id: requireString(environment.id, "environment.id"),
		projectId: requireString(environment.projectId, "environment.projectId"),
		name: requireString(environment.name, "environment.name"),
		kind: "persistent",
		isProduction: environment.isProduction,
		autoDeploy: environment.autoDeploy,
		copiedFromEnvironmentId: environment.copiedFromEnvironmentId || undefined,
		createdAt: optionalDate(environment.createdAt),
		updatedAt: optionalDate(environment.updatedAt),
	};
}

export function toEnvironments(
	environments: Environment[],
): DashboardEnvironment[] {
	return environments.map(toEnvironment);
}

export function toRepositoryInspection(response: {
	accessState: number;
	defaultBranch: string;
	dockerfileCandidates: string[];
	recommendedBuildRecipe?: BuildRecipe;
	recommendedPorts: number[];
}): DashboardRepositoryInspection {
	return {
		accessState: enumName(
			SourceAccessStateSchema,
			response.accessState,
		) as DashboardRepositoryInspection["accessState"],
		defaultBranch: response.defaultBranch,
		dockerfileCandidates: response.dockerfileCandidates,
		recommendedBuildRecipe: toBuildRecipe(response.recommendedBuildRecipe),
		recommendedPorts: response.recommendedPorts,
	};
}

function toBuildRecipe(
	recipe: BuildRecipe | undefined,
): DashboardBuildRecipe | undefined {
	if (!recipe || (recipe.dockerfilePath === "" && recipe.contextDir === "")) {
		return undefined;
	}
	return {
		dockerfilePath: recipe.dockerfilePath,
		contextDir: recipe.contextDir,
	};
}

export function toDomainBinding(
	binding: DomainBinding,
): DashboardDomainBinding {
	return {
		hostname: requireString(binding.hostname, "domain binding.hostname"),
		serviceId: requireString(binding.serviceId, "domain binding.serviceId"),
		targetPort: binding.targetPort,
		platformGenerated: binding.platformGenerated,
		ownershipState: enumName(
			DomainOwnershipStateSchema,
			binding.ownershipState,
		) as DashboardDomainOwnershipState,
		ownershipMessage: binding.ownershipMessage || undefined,
	};
}

export function toDomainBindings(
	bindings: DomainBinding[],
): DashboardDomainBinding[] {
	return bindings.map(toDomainBinding);
}

export function toServiceRecord(service: Service): DashboardServiceRecord {
	return {
		id: requireString(service.id, "service.id"),
		environmentId: requireString(
			service.environmentId,
			"service.environmentId",
		),
		name: requireString(service.name, "service.name"),
		internalHostname: service.internalHostname || undefined,
		specRevision: safeNumber(service.specRevision) || undefined,
		rolloutGeneration: safeNumber(service.rolloutGeneration) || undefined,
		createdAt: optionalDate(service.createdAt),
		updatedAt: optionalDate(service.updatedAt),
		spec: toServiceSpec(service.spec),
		sourceSummary: toServiceSourceSummary(service.sourceSummary),
		lastSuccessfulCommitSha: service.lastSuccessfulCommitSha || undefined,
		resolvedImage: service.resolvedImage || undefined,
		latestBuild: toBuildStatus(service.latestBuild),
		latestDeployment: toDeploymentStatus(service.latestDeployment),
		pendingChanges: service.pendingChanges,
		unappliedChangeCount: safeNumber(service.unappliedChangeCount),
		unappliedChanges: service.unappliedChanges.map(toUnappliedChange),
		desiredReplicaCount: safeNumber(service.desiredReplicaCount, 1),
		readyReplicaCount: safeNumber(service.readyReplicaCount),
		placementMessage: service.placementMessage || undefined,
	};
}

function toUnappliedChange(
	change: ServiceUnappliedChange,
): DashboardUnappliedChange {
	return {
		id: change.id,
		section: change.section,
		field: change.field,
		path: change.path,
		action: enumName(
			ServiceUnappliedChangeActionSchema,
			change.action,
		) as DashboardUnappliedChangeAction,
		currentValue: change.currentValue,
		newValue: change.newValue,
	};
}

export function toBuildStatus(
	build: BuildStatus | undefined,
): DashboardBuildStatus | undefined {
	if (!build?.buildId) {
		return undefined;
	}
	return {
		buildId: build.buildId,
		state: enumName(BuildStateSchema, build.state) as DashboardBuildState,
		commitSha: build.commitSha,
		imageDigest: build.imageDigest,
		queuedAt: optionalDate(build.queuedAt),
		startedAt: optionalDate(build.startedAt),
		finishedAt: optionalDate(build.finishedAt),
		failureReason: build.failureReason,
		commitMessage: build.commitMessage || undefined,
		commitAuthor: build.commitAuthor || undefined,
		stages: build.stages.map(toDeploymentStage),
	};
}

function toDeploymentStage(stage: DeploymentStage): DashboardDeploymentStage {
	return {
		key: stage.key,
		label: stage.label,
		detail: stage.detail,
		state: enumName(
			DeploymentStageStateSchema,
			stage.state,
		) as DashboardDeploymentStageState,
		startedAt: optionalDate(stage.startedAt),
		finishedAt: optionalDate(stage.finishedAt),
	};
}

export function toDeploymentStatus(
	status: DeploymentStatus | undefined,
): DashboardDeploymentStatus | undefined {
	if (!status) {
		return undefined;
	}
	return {
		deploymentId: status.deploymentId,
		state: enumName(
			DeploymentStateSchema,
			status.state,
		) as DashboardDeploymentState,
		transitionedAt: optionalDate(status.transitionedAt),
		causeKind: enumName(
			DeploymentCauseKindSchema,
			status.causeKind,
		) as DashboardDeploymentCauseKind,
		causeId: status.causeId,
		reasonCode: status.reasonCode,
		detail: status.detail,
		specRevision: safeNumber(status.specRevision),
		imageDigest: status.imageDigest,
		rolloutGeneration: safeNumber(status.rolloutGeneration),
	};
}

const dashboardDeploymentActions: readonly DashboardDeploymentAction[] = [
	"DEPLOYMENT_ACTION_RESTART",
	"DEPLOYMENT_ACTION_EXACT_REDEPLOY",
	"DEPLOYMENT_ACTION_ROLLBACK",
	"DEPLOYMENT_ACTION_CANCEL",
	"DEPLOYMENT_ACTION_REMOVE",
	"DEPLOYMENT_ACTION_RETRY",
];

export function toDeploymentRecord(
	record: DeploymentRecord,
): DashboardDeploymentRecord {
	return {
		id: requireString(record.id, "deployment record.id"),
		rolloutGeneration: safeNumber(record.rolloutGeneration),
		specRevision: safeNumber(record.specRevision) || undefined,
		createdAt: optionalDate(record.createdAt),
		build: toBuildStatus(record.build),
		isCurrent: record.isCurrent,
		status: toDeploymentStatus(record.status),
		stages: record.stages.map(toDeploymentStage),
		imageDigest: record.imageDigest,
		actions: record.actions.map(toDeploymentActionRecord),
		variableVersions: Object.fromEntries(
			Object.entries(record.variableVersions).map(([key, version]) => [
				key,
				safeNumber(version),
			]),
		),
	};
}

function toDeploymentActionRecord(
	record: DeploymentActionRecord,
): DashboardDeploymentActionRecord {
	const action = enumName(
		DeploymentActionSchema,
		record.action,
	) as DashboardDeploymentAction;
	if (!dashboardDeploymentActions.includes(action)) {
		throw new Error(`invalid deployment action: ${record.action}`);
	}
	return {
		id: requireString(record.id, "deployment action.id"),
		action,
		targetDeploymentId: record.targetDeploymentId,
		resultDeploymentId: record.resultDeploymentId || undefined,
		allocationId: record.allocationId || undefined,
		requestedByUserId: record.requestedByUserId,
		createdAt: optionalDate(record.createdAt),
	};
}

export function toServiceSpec(
	spec: ServiceSpec | undefined,
): DashboardServiceSpec | undefined {
	const runtime = toRuntimeSpec(spec?.runtime);
	const source = toSourceSpec(
		spec?.source?.source.case === "sourceSpec"
			? spec.source.source.value
			: undefined,
	);
	if (
		!source &&
		runtime.ports.length === 0 &&
		Object.keys(runtime.env).length === 0
	) {
		return undefined;
	}
	const desiredReplicaCount = spec?.desiredReplicaCount;
	const placementRegion = spec?.placementRegion;
	const rollingStrategy = toRollingStrategy(spec?.rollingStrategy);
	return {
		source,
		runtime,
		...(desiredReplicaCount === undefined ? {} : { desiredReplicaCount }),
		...(placementRegion ? { placementRegion } : {}),
		...(rollingStrategy === undefined ? {} : { rollingStrategy }),
	};
}

function toRollingStrategy(
	rolling:
		| { healthcheckTimeoutSeconds?: number; drainingSeconds?: number }
		| undefined,
): DashboardRollingStrategy | undefined {
	if (!rolling) {
		return undefined;
	}
	return {
		healthcheckTimeoutSeconds: rolling.healthcheckTimeoutSeconds ?? 300,
		drainingSeconds: rolling.drainingSeconds ?? 0,
	};
}

function toServiceSourceSummary(
	summary: ServiceSourceSummary | undefined,
): DashboardServiceSourceSummary | undefined {
	if (!summary || summary.source.case !== "sourceState") {
		return undefined;
	}
	const state: SourceStateSummary = summary.source.value;
	return {
		desiredSpec: toSourceSpec(state.desiredSpec),
		resolvedBinding: toResolvedSourceBinding(state.resolvedBinding),
		latestRevision: toSourceRevision(state.latestRevision),
	};
}

function toSourceRevision(
	revision: SourceRevision | undefined,
): DashboardSourceRevision | undefined {
	if (!revision?.commitSha) {
		return undefined;
	}
	return {
		commitSha: revision.commitSha,
		observedAt: optionalDate(revision.observedAt),
	};
}

function toSourceSpec(
	spec: ServiceSourceSpec | undefined,
): DashboardSourceSpec | undefined {
	if (!spec) {
		return undefined;
	}
	return {
		provider: spec.provider,
		repositorySelector: spec.repositorySelector,
		trackedRef: spec.trackedRef,
		buildRecipe: toBuildRecipe(spec.buildRecipe),
	};
}

function toResolvedSourceBinding(
	binding: ResolvedSourceBinding | undefined,
): DashboardResolvedSourceBinding | undefined {
	if (!binding) {
		return undefined;
	}
	return {
		repositorySelector: binding.repositorySelector,
		trackedRef: binding.trackedRef,
		accessState: enumName(
			SourceAccessStateSchema,
			binding.accessState,
		) as DashboardResolvedSourceBinding["accessState"],
		buildRecipe: toBuildRecipe(binding.buildRecipe),
	};
}

export function toRuntimeSpec(
	runtime: ServiceRuntime | undefined,
): DashboardRuntimeSpec {
	return {
		env: runtime?.env ?? {},
		cpuMillis: positiveResource(runtime?.cpuMillis, DEFAULT_SERVICE_CPU_MILLIS),
		memoryMebibytes: positiveResource(
			runtime?.memoryMebibytes,
			DEFAULT_SERVICE_MEMORY_MEBIBYTES,
		),
		ports: (runtime?.ports ?? [])
			.map(toRuntimePort)
			.filter((port): port is DashboardRuntimePort => port !== undefined),
		healthCheck: toHTTPHealthCheck(runtime?.healthCheck),
		livenessCheck: toHTTPHealthCheck(runtime?.livenessCheck),
		restart: toRestartSpec(runtime?.restart),
		volumeName: runtime?.volumeName || undefined,
	};
}

function positiveResource(
	value: bigint | number | undefined,
	fallback: number,
): number {
	const converted = safeNumber(value);
	return converted > 0 ? converted : fallback;
}

function toRuntimePort(
	port: ServiceRuntimePort,
): DashboardRuntimePort | undefined {
	if (!Number.isInteger(port.port) || port.port < 1 || port.port > 65535) {
		return undefined;
	}
	return { port: port.port, primary: port.primary };
}

function toHTTPHealthCheck(
	check: HealthCheck | undefined,
): DashboardHTTPHealthCheck | undefined {
	if (!check || check.type !== HealthCheck_Type.HTTP || check.path === "") {
		return undefined;
	}
	return {
		path: check.path,
		port: check.port || undefined,
		timeoutSeconds: check.timeoutSeconds || undefined,
	};
}

function toRestartSpec(
	restart: ServiceRestart | undefined,
): DashboardRestartSpec | undefined {
	if (!restart) {
		return undefined;
	}
	return {
		policy: restartPolicyName(restart.policy),
		maxRestarts: restart.maxRestarts,
		windowSeconds: restart.windowSeconds,
		initialDelayMs: restart.initialDelayMs,
		maxDelayMs: restart.maxDelayMs,
		backoffMultiplier: restart.backoffMultiplier,
		jitter: restart.jitter,
		stableAfterSeconds: restart.stableAfterSeconds,
	};
}

function restartPolicyName(policy: number): DashboardRestartSpec["policy"] {
	const name = enumName(RestartPolicySchema, policy);
	switch (name) {
		case "RESTART_POLICY_ALWAYS":
		case "RESTART_POLICY_NEVER":
			return name;
		default:
			return "RESTART_POLICY_ON_FAILURE";
	}
}

export function toServiceStatus(status: ServiceStatus): DashboardServiceStatus {
	if (!status.service) {
		throw new Error("invalid service status: expected a service");
	}
	return {
		service: toServiceRecord(status.service),
		allocation: toAllocationStatus(status.allocation),
		allocations: status.allocations
			.map(toAllocationStatus)
			.filter(
				(allocation): allocation is DashboardAllocationStatus =>
					allocation !== undefined,
			),
	};
}

export function toIndexedServiceStatus(
	response: ServiceStatus,
): DashboardIndexedServiceStatus {
	return {
		index: safeNumber(response.index),
		notModified: response.notModified,
		status: response.notModified ? undefined : toServiceStatus(response),
	};
}

export function toIndexedServices(
	response: ListServicesResponse,
): DashboardIndexedServices {
	return {
		index: safeNumber(response.index),
		notModified: response.notModified,
		services: response.notModified
			? undefined
			: response.services.map(toServiceRecord),
	};
}

function toAllocationStatus(
	allocation: AllocationStatus | undefined,
): DashboardAllocationStatus | undefined {
	if (!allocation) {
		return undefined;
	}
	return {
		allocationId: allocation.allocationId,
		serviceId: allocation.serviceId,
		agentId: allocation.agentId,
		desiredSpecRevision: safeNumber(allocation.desiredSpecRevision),
		appliedSpecRevision: safeNumber(allocation.appliedSpecRevision),
		phase: allocation.phase,
		message: allocation.message,
		allocationIpv4: allocation.allocationIpv4,
		allocationIpv6: allocation.allocationIpv6,
		healthy: allocation.healthy,
		updatedAt: optionalDate(allocation.updatedAt),
		desiredRolloutGeneration: safeNumber(allocation.desiredRolloutGeneration),
		appliedRolloutGeneration: safeNumber(allocation.appliedRolloutGeneration),
		healthyIpv4Ports: allocation.healthyIpv4Ports,
		healthyIpv6Ports: allocation.healthyIpv6Ports,
		operatorRestartNonce: safeNumber(allocation.operatorRestartNonce),
		rolloutState: allocation.rolloutState || undefined,
		drainStartedAt: optionalDate(allocation.drainStartedAt),
		drainDeadline: optionalDate(allocation.drainDeadline),
		restart: toRestartObservation(allocation.restart),
	};
}

function toRestartObservation(
	observation: RestartObservation | undefined,
): DashboardAllocationStatus["restart"] {
	if (!observation) {
		return undefined;
	}
	return {
		restartCount: safeNumber(observation.restartCount),
		crashLoop: observation.crashLoop,
		lastCause: enumName(RestartCauseSchema, observation.lastCause),
		message: observation.message,
	};
}

export function toServiceLogLine(
	line: ServiceLogLine,
): DashboardServiceLogLine {
	return {
		observedAt: optionalDate(line.observedAt),
		allocationId: line.allocationId,
		agentId: line.agentId,
		stream: line.stream,
		rolloutGeneration: safeNumber(line.rolloutGeneration),
		sequence: safeNumber(line.sequence),
		line: line.line,
		logType: enumName(
			ServiceLogTypeSchema,
			line.logType,
		) as DashboardServiceLogType,
		buildId: line.buildId || undefined,
		stage: line.stage || undefined,
	};
}

export function toServiceLogLines(
	response: ListServiceLogsResponse,
): DashboardServiceLogLine[] {
	return response.lines.map(toServiceLogLine);
}

export function toDeploymentRecords(
	response: ListServiceDeploymentsResponse,
): DashboardDeploymentRecord[] {
	return response.deployments.map(toDeploymentRecord);
}

export function toFleet(fleet: Fleet): DashboardFleet {
	return {
		agents: fleet.agents.map(toFleetAgent),
		capacity: {
			nodeCount: safeNumber(fleet.capacity?.nodeCount),
			schedulableNodeCount: safeNumber(fleet.capacity?.schedulableNodeCount),
			schedulableCpuMillis: safeNumber(fleet.capacity?.schedulableCpuMillis),
			schedulableMemoryMebibytes: safeNumber(
				fleet.capacity?.schedulableMemoryMebibytes,
			),
			allocatedCpuMillis: safeNumber(fleet.capacity?.allocatedCpuMillis),
			allocatedMemoryMebibytes: safeNumber(
				fleet.capacity?.allocatedMemoryMebibytes,
			),
			headroomCpuMillis: safeNumber(fleet.capacity?.headroomCpuMillis),
			headroomMemoryMebibytes: safeNumber(
				fleet.capacity?.headroomMemoryMebibytes,
			),
		},
		versionWarning: fleet.versionWarning || undefined,
	};
}

export function toFleetAgent(agent: Agent): DashboardFleetAgent {
	return {
		id: requireString(agent.id, "fleet agent.id"),
		name: requireString(agent.name, "fleet agent.name"),
		lifecycleState: enumName(
			AgentLifecycleStateSchema,
			agent.lifecycleState,
		) as DashboardAgentLifecycleState,
		region: agent.region,
		zone: agent.zone,
		failureDomain: agent.failureDomain,
		healthy: agent.healthy,
		lastSeenAt: optionalDate(agent.lastSeenAt),
		cpuMillisCapacity: safeNumber(agent.cpuMillisCapacity),
		memoryMebibytesCapacity: safeNumber(agent.memoryMebibytesCapacity),
		reservedCpuMillis: safeNumber(agent.reservedCpuMillis),
		reservedMemoryMebibytes: safeNumber(agent.reservedMemoryMebibytes),
		schedulableCpuMillis: safeNumber(agent.schedulableCpuMillis),
		schedulableMemoryMebibytes: safeNumber(agent.schedulableMemoryMebibytes),
		allocatedCpuMillis: safeNumber(agent.allocatedCpuMillis),
		allocatedMemoryMebibytes: safeNumber(agent.allocatedMemoryMebibytes),
		headroomCpuMillis: safeNumber(agent.headroomCpuMillis),
		headroomMemoryMebibytes: safeNumber(agent.headroomMemoryMebibytes),
		allocationCount: safeNumber(agent.allocationCount),
		runtimeCapabilities: agent.runtimeCapabilities,
		softwareVersion: agent.softwareVersion,
		versionSkewWarning: agent.versionSkewWarning || undefined,
		maintenanceMessage: agent.maintenanceMessage || undefined,
		credentialRevokedAt: optionalDate(agent.credentialRevokedAt),
	};
}

export function toAgentEnrollment(
	enrollment: AgentEnrollment,
): DashboardAgentEnrollment {
	if (!enrollment.agent) {
		throw new Error("invalid agent enrollment: expected an agent");
	}
	return {
		agent: toFleetAgent(enrollment.agent),
		bootstrapToken: requireString(
			enrollment.bootstrapToken,
			"agent enrollment.bootstrapToken",
		),
	};
}

export function toListServiceLogsRequest(input: {
	serviceId: string;
	allocationId?: string;
	limit?: number;
	logType?: DashboardServiceLogType;
	buildId?: string;
	search?: string;
	startTime?: Date;
	endTime?: Date;
}): ListServiceLogsRequest {
	return create(ListServiceLogsRequestSchema, {
		serviceId: input.serviceId,
		allocationId: input.allocationId,
		startTime: input.startTime ? timestampFromDate(input.startTime) : undefined,
		endTime: input.endTime ? timestampFromDate(input.endTime) : undefined,
		limit: input.limit,
		logType: input.logType
			? enumNumber(ServiceLogTypeSchema, input.logType)
			: 0,
		buildId: input.buildId,
		search: input.search,
	});
}

export function toEmptyRequest() {
	return create(EmptySchema);
}

export function toCreateProjectRequest(name: string) {
	return create(CreateProjectRequestSchema, { name });
}

export function toListEnvironmentsRequest(projectId: string) {
	return create(ListEnvironmentsRequestSchema, { projectId });
}

export function toGetEnvironmentRequest(environmentId: string) {
	return create(GetEnvironmentRequestSchema, { environmentId });
}

export function toCreateEnvironmentRequest(input: {
	projectId: string;
	name: string;
}) {
	return create(CreateEnvironmentRequestSchema, input);
}

export function toDuplicateEnvironmentRequest(input: {
	sourceEnvironmentId: string;
	name: string;
	copyVariables: boolean;
}) {
	return create(DuplicateEnvironmentRequestSchema, input);
}

export function toRenameEnvironmentRequest(input: {
	environmentId: string;
	name: string;
}) {
	return create(RenameEnvironmentRequestSchema, input);
}

export function toUpdateEnvironmentAutoDeployRequest(input: {
	environmentId: string;
	autoDeploy: boolean;
}) {
	return create(UpdateEnvironmentAutoDeployRequestSchema, input);
}

export function toDeleteEnvironmentRequest(environmentId: string) {
	return create(DeleteEnvironmentRequestSchema, { environmentId });
}

export function toReleaseEnvironmentRequest(environmentId: string) {
	return create(ReleaseEnvironmentRequestSchema, { environmentId });
}

export function toInspectSourceRequest(input: {
	projectId: string;
	provider: string;
	repositorySelector: string;
	githubUserAccessToken: string;
}) {
	return create(InspectSourceRequestSchema, input);
}

export function toLinkGitHubRepositoryRequest(input: {
	projectId: string;
	repositorySelector: string;
	githubUserAccessToken: string;
}) {
	return create(LinkGitHubRepositoryRequestSchema, input);
}

export function toScaleServiceRequest(input: {
	serviceId: string;
	desiredReplicaCount: number;
}) {
	return create(ScaleServiceRequestSchema, input);
}

export function toDiscardServiceChangesRequest(input: {
	serviceId: string;
	changeIds?: string[];
	discardAll?: boolean;
}) {
	return create(DiscardServiceChangesRequestSchema, input);
}

export function toDeleteServiceRequest(serviceId: string) {
	return create(DeleteServiceRequestSchema, { serviceId });
}

export function toGetServiceRequest(serviceId: string) {
	return create(GetServiceRequestSchema, { serviceId });
}

export function toListServiceDeploymentsRequest(input: {
	serviceId: string;
	limit?: number;
}) {
	return create(ListServiceDeploymentsRequestSchema, input);
}

export function toListDomainBindingsRequest(serviceId: string) {
	return create(ListDomainBindingsRequestSchema, { serviceId });
}

export function toGenerateDomainBindingRequest(input: {
	serviceId: string;
	targetPort: number;
}) {
	return create(GenerateDomainBindingRequestSchema, input);
}

export function toCreateDomainBindingRequest(input: {
	serviceId: string;
	hostname: string;
	targetPort: number;
}) {
	return create(CreateDomainBindingRequestSchema, {
		binding: input,
	});
}

export function toUpdateDomainBindingRequest(input: {
	serviceId: string;
	hostname: string;
	targetPort: number;
}) {
	return create(UpdateDomainBindingRequestSchema, {
		hostname: input.hostname,
		binding: { serviceId: input.serviceId, targetPort: input.targetPort },
	});
}

export function toDeleteDomainBindingRequest(hostname: string) {
	return create(DeleteDomainBindingRequestSchema, { hostname });
}

export function toListServicesRequest(input: {
	environmentId: string;
	waitIndex?: number;
	waitTimeoutSeconds?: number;
}) {
	return {
		environmentId: input.environmentId,
		waitIndex:
			input.waitIndex === undefined ? undefined : BigInt(input.waitIndex),
		waitTimeoutSeconds: input.waitTimeoutSeconds,
	};
}

export function toGetServiceStatusRequest(input: {
	serviceId: string;
	waitIndex?: number;
	waitTimeoutSeconds?: number;
}) {
	return {
		serviceId: input.serviceId,
		waitIndex:
			input.waitIndex === undefined ? undefined : BigInt(input.waitIndex),
		waitTimeoutSeconds: input.waitTimeoutSeconds,
	};
}

export function toApplyDeploymentActionRequest(input: {
	serviceId: string;
	deploymentId: string;
	action: DashboardDeploymentAction;
	idempotencyKey: string;
	allocationId?: string;
}) {
	return create(ApplyDeploymentActionRequestSchema, {
		serviceId: input.serviceId,
		deploymentId: input.deploymentId,
		action: enumNumber(DeploymentActionSchema, input.action),
		idempotencyKey: input.idempotencyKey,
		allocationId: input.allocationId,
	});
}

export function toUpdateServiceRequest(input: {
	serviceId: string;
	name?: string;
	spec: DashboardServiceSpec;
}): UpdateServiceRequest {
	return create(UpdateServiceRequestSchema, {
		serviceId: input.serviceId,
		service: {
			name: input.name,
			spec: toProtoServiceSpec(input.spec),
		},
	});
}

export function toCreateServiceRequest(input: {
	environmentId: string;
	name: string;
	spec: DashboardServiceSpec;
}): ReturnType<typeof create<typeof CreateServiceRequestSchema>> {
	return create(CreateServiceRequestSchema, {
		environmentId: input.environmentId,
		service: {
			name: input.name,
			spec: toProtoServiceSpec(input.spec),
		},
	});
}

function toProtoServiceSpec(spec: DashboardServiceSpec) {
	return {
		runtime: toProtoRuntimeSpec(spec.runtime),
		source: {
			source: {
				case: "sourceSpec" as const,
				value: {
					provider: spec.source?.provider ?? "",
					repositorySelector: spec.source?.repositorySelector ?? "",
					trackedRef: spec.source?.trackedRef ?? "",
					buildRecipe: spec.source?.buildRecipe
						? {
								dockerfilePath: spec.source.buildRecipe.dockerfilePath,
								contextDir: spec.source.buildRecipe.contextDir,
							}
						: undefined,
				},
			},
		},
		desiredReplicaCount: spec.desiredReplicaCount,
		placementRegion: spec.placementRegion,
		rollingStrategy: spec.rollingStrategy,
	};
}

function toProtoRuntimeSpec(runtime: DashboardRuntimeSpec) {
	return {
		env: runtime.env ?? {},
		cpuMillis: BigInt(Math.trunc(runtime.cpuMillis)),
		memoryMebibytes: BigInt(Math.trunc(runtime.memoryMebibytes)),
		ports: (runtime.ports ?? []).map((port) => ({
			port: port.port,
			primary: port.primary,
		})),
		healthCheck: runtime.healthCheck
			? {
					type: HealthCheck_Type.HTTP,
					path: runtime.healthCheck.path,
					port: runtime.healthCheck.port,
					timeoutSeconds: runtime.healthCheck.timeoutSeconds,
				}
			: undefined,
		livenessCheck: runtime.livenessCheck
			? {
					type: HealthCheck_Type.HTTP,
					path: runtime.livenessCheck.path,
					port: runtime.livenessCheck.port,
					timeoutSeconds: runtime.livenessCheck.timeoutSeconds,
				}
			: undefined,
		restart: toProtoRestartSpec(runtime.restart),
		volumeName: runtime.volumeName,
	};
}

function toProtoRestartSpec(restart: DashboardRestartSpec | undefined) {
	if (!restart) {
		return undefined;
	}
	return {
		policy: enumNumber(RestartPolicySchema, restart.policy),
		maxRestarts: restart.maxRestarts,
		windowSeconds: restart.windowSeconds,
		initialDelayMs: restart.initialDelayMs,
		maxDelayMs: restart.maxDelayMs,
		backoffMultiplier: restart.backoffMultiplier,
		jitter: restart.jitter,
		stableAfterSeconds: restart.stableAfterSeconds,
	};
}

export function toIngestGitHubWebhookRequest(input: {
	deliveryId: string;
	eventType: string;
	signature256: string;
	payload: Uint8Array;
}): IngestGitHubWebhookRequest {
	return create(IngestGitHubWebhookRequestSchema, {
		deliveryId: input.deliveryId,
		eventType: input.eventType,
		signature256: input.signature256,
		payload: input.payload,
	});
}

export function toCreateAgentRequest(input: {
	agentId: string;
	name: string;
	region: string;
	zone: string;
	failureDomain: string;
	reservedCpuMillis: number;
	reservedMemoryMebibytes: number;
}): CreateAgentRequest {
	return create(CreateAgentRequestSchema, {
		agentId: input.agentId,
		name: input.name,
		region: input.region,
		zone: input.zone,
		failureDomain: input.failureDomain,
		reservedCpuMillis: BigInt(Math.trunc(input.reservedCpuMillis)),
		reservedMemoryMebibytes: BigInt(Math.trunc(input.reservedMemoryMebibytes)),
	});
}

export function toUpdateAgentRequest(input: {
	agentId: string;
	name: string;
	region: string;
	zone: string;
	failureDomain: string;
	reservedCpuMillis: number;
	reservedMemoryMebibytes: number;
}) {
	return create(UpdateAgentRequestSchema, {
		agentId: input.agentId,
		name: input.name,
		region: input.region,
		zone: input.zone,
		failureDomain: input.failureDomain,
		reservedCpuMillis: BigInt(Math.trunc(input.reservedCpuMillis)),
		reservedMemoryMebibytes: BigInt(Math.trunc(input.reservedMemoryMebibytes)),
	});
}

export function toSetAgentLifecycleRequest(input: {
	agentId: string;
	lifecycleState: DashboardAgentLifecycleState;
}): SetAgentLifecycleRequest {
	return create(SetAgentLifecycleRequestSchema, {
		agentId: input.agentId,
		lifecycleState: enumNumber(AgentLifecycleStateSchema, input.lifecycleState),
	});
}
