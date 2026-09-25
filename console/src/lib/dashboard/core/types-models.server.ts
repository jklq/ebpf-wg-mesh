export interface DashboardUser {
	id: string;
	email: string;
}

export type DashboardProjectKind = "PROJECT_KIND_USER" | "PROJECT_KIND_MANAGED";

/**
 * A tombstoned resource. Absent while the resource is live. `inherited` means
 * an ancestor was deleted and the timestamps identify that ancestor's
 * tombstone.
 */
export interface DashboardDeletionState {
	deletedAt?: Date;
	deleteExpiresAt?: Date;
	inherited: boolean;
}

export interface DashboardProject {
	id: string;
	name: string;
	kind: DashboardProjectKind;
	systemKey?: string;
	/** Log retention in days; 0 or absent means the platform default. */
	logRetentionDays?: number;
	deletion?: DashboardDeletionState;
}

/** Live dependents a delete would tombstone, as previewed by the platform. */
export interface DashboardDeletionPreview {
	environments: Array<{ id: string; name: string; isProduction: boolean }>;
	services: Array<{
		id: string;
		name: string;
		environmentId: string;
		environmentName: string;
	}>;
	domains: Array<{
		hostname: string;
		serviceId: string;
		serviceName: string;
		platformGenerated: boolean;
	}>;
	volumes: Array<{ id: string; name: string; environmentId: string }>;
}

export interface DashboardVolume {
	id: string;
	environmentId: string;
	name: string;
	sizeBytes: number;
	createdAt?: Date;
	deletion?: DashboardDeletionState;
}

export type DashboardDeletableKind = "project" | "environment" | "volume";

export type DashboardRestorableKind =
	| "project"
	| "environment"
	| "service"
	| "domain";

export type DashboardDeletedResourceKind = DashboardRestorableKind | "volume";

/**
 * One tombstoned resource on the recently deleted view. `id` is the hostname
 * for domains. `deletedWith` names the nearest tombstoned ancestor: such a
 * resource comes back with (or only after) that ancestor.
 */
export interface DashboardDeletedResource {
	kind: DashboardDeletedResourceKind;
	id: string;
	name: string;
	projectId: string;
	projectName: string;
	environmentName?: string;
	serviceName?: string;
	deletion: DashboardDeletionState;
	deletedWith?: {
		kind: DashboardDeletedResourceKind;
		id: string;
		name: string;
	};
}

export interface DashboardProjectSettings {
	project: DashboardProject;
	environments: Array<{
		environment: DashboardEnvironment;
		volumes: Array<DashboardVolume>;
	}>;
}

export interface DashboardEnvironment {
	id: string;
	projectId: string;
	name: string;
	kind: "persistent";
	isProduction: boolean;
	autoDeploy: boolean;
	copiedFromEnvironmentId?: string;
	createdAt?: Date;
	updatedAt?: Date;
	deletion?: DashboardDeletionState;
}

export type RepositoryAccessState =
	| "SOURCE_ACCESS_STATE_AVAILABLE"
	| "SOURCE_ACCESS_STATE_INSTALLATION_REQUIRED"
	| "SOURCE_ACCESS_STATE_ACCESS_REVOKED"
	| "SOURCE_ACCESS_STATE_REPOSITORY_DELETED"
	| "SOURCE_ACCESS_STATE_UNSPECIFIED";

export type DashboardBuildState =
	| "BUILD_STATE_QUEUED"
	| "BUILD_STATE_RUNNING"
	| "BUILD_STATE_SUCCEEDED"
	| "BUILD_STATE_FAILED"
	| "BUILD_STATE_SUPERSEDED"
	| "BUILD_STATE_CANCELLED"
	| "BUILD_STATE_UNSPECIFIED";

export type DashboardDeploymentAction =
	| "DEPLOYMENT_ACTION_RESTART"
	| "DEPLOYMENT_ACTION_EXACT_REDEPLOY"
	| "DEPLOYMENT_ACTION_ROLLBACK"
	| "DEPLOYMENT_ACTION_CANCEL"
	| "DEPLOYMENT_ACTION_REMOVE"
	| "DEPLOYMENT_ACTION_RETRY";

export interface DashboardDeploymentActionRecord {
	id: string;
	action: DashboardDeploymentAction;
	targetDeploymentId: string;
	resultDeploymentId?: string;
	allocationId?: string;
	requestedByUserId: string;
	createdAt?: Date;
}

export type DashboardDeploymentStageState =
	| "DEPLOYMENT_STAGE_STATE_PENDING"
	| "DEPLOYMENT_STAGE_STATE_RUNNING"
	| "DEPLOYMENT_STAGE_STATE_SUCCEEDED"
	| "DEPLOYMENT_STAGE_STATE_FAILED"
	| "DEPLOYMENT_STAGE_STATE_SKIPPED"
	| "DEPLOYMENT_STAGE_STATE_UNSPECIFIED";

export type DashboardDeploymentState =
	| "DEPLOYMENT_STATE_STAGED"
	| "DEPLOYMENT_STATE_QUEUED_BUILD"
	| "DEPLOYMENT_STATE_BUILDING"
	| "DEPLOYMENT_STATE_SCHEDULING"
	| "DEPLOYMENT_STATE_IMAGE_PULL"
	| "DEPLOYMENT_STATE_STARTING"
	| "DEPLOYMENT_STATE_READINESS"
	| "DEPLOYMENT_STATE_ACTIVE"
	| "DEPLOYMENT_STATE_DRAINING"
	| "DEPLOYMENT_STATE_COMPLETED"
	| "DEPLOYMENT_STATE_FAILED"
	| "DEPLOYMENT_STATE_CANCELLED"
	| "DEPLOYMENT_STATE_CRASHED"
	| "DEPLOYMENT_STATE_REMOVED"
	| "DEPLOYMENT_STATE_SUPERSEDED"
	| "DEPLOYMENT_STATE_UNSPECIFIED";

export type DashboardDeploymentCauseKind =
	| "DEPLOYMENT_CAUSE_KIND_USER"
	| "DEPLOYMENT_CAUSE_KIND_SYSTEM"
	| "DEPLOYMENT_CAUSE_KIND_AGENT"
	| "DEPLOYMENT_CAUSE_KIND_BUILDER"
	| "DEPLOYMENT_CAUSE_KIND_WEBHOOK"
	| "DEPLOYMENT_CAUSE_KIND_UNSPECIFIED";

export interface DashboardDeploymentStatus {
	deploymentId: string;
	state: DashboardDeploymentState;
	transitionedAt?: Date;
	causeKind: DashboardDeploymentCauseKind;
	causeId: string;
	reasonCode: string;
	detail: string;
	specRevision: number;
	imageDigest: string;
	rolloutGeneration: number;
	/** The deployment reused an image built earlier instead of building. */
	buildReused?: boolean;
}

export type DashboardServiceLogType =
	| "SERVICE_LOG_TYPE_RUNTIME"
	| "SERVICE_LOG_TYPE_BUILD"
	| "SERVICE_LOG_TYPE_DEPLOY"
	| "SERVICE_LOG_TYPE_HTTP"
	| "SERVICE_LOG_TYPE_NETWORK"
	| "SERVICE_LOG_TYPE_UNSPECIFIED";

export type DashboardBuilderKind =
	| "BUILDER_KIND_RAILPACK"
	| "BUILDER_KIND_DOCKERFILE";

export interface DashboardBuildRecipe {
	builder?: DashboardBuilderKind;
	dockerfilePath: string;
	contextDir: string;
}

export interface DashboardGitHubAccount {
	providerSubject: string;
	login: string;
	primaryEmail: string;
	tokenType: string;
	scope: string;
}

export interface StoredDashboardGitHubAccount extends DashboardGitHubAccount {
	accessToken: string;
	tokenVersion: number;
	accessTokenExpiresAt?: Date;
	refreshToken?: string;
	refreshTokenExpiresAt?: Date;
}

export interface DashboardOnboardingDraft {
	projectId: string;
	environmentId: string;
	serviceId: string;
	repositorySelector: string;
	trackedRef: string;
	builder: string;
	dockerfilePath: string;
	contextDir: string;
	hostname: string;
}

export interface GitHubUserRepository {
	owner: string;
	name: string;
	fullName: string;
	private: boolean;
	defaultBranch: string;
}

export interface DashboardRepositoryInspection {
	accessState: RepositoryAccessState;
	defaultBranch: string;
	dockerfileCandidates: Array<string>;
	recommendedBuildRecipe?: DashboardBuildRecipe;
	recommendedDockerfileRecipe?: DashboardBuildRecipe;
	recommendedPorts: number[];
	detectedLanguage: string;
	detectedStartCommand: string;
	analysisError: string;
}

export interface DashboardSourceSpec {
	provider: string;
	repositorySelector: string;
	trackedRef: string;
	buildRecipe?: DashboardBuildRecipe;
}

export interface DashboardRuntimePort {
	port: number;
	primary: boolean;
}

export interface DashboardHTTPHealthCheck {
	path: string;
	port?: number;
	timeoutSeconds?: number;
}

export type DashboardRestartPolicy =
	| "RESTART_POLICY_ALWAYS"
	| "RESTART_POLICY_ON_FAILURE"
	| "RESTART_POLICY_NEVER";

export interface DashboardRestartSpec {
	policy: DashboardRestartPolicy;
	maxRestarts?: number;
	windowSeconds?: number;
	initialDelayMs?: number;
	maxDelayMs?: number;
	backoffMultiplier?: number;
	jitter?: number;
	stableAfterSeconds?: number;
}

export {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "./defaults";

export interface DashboardRuntimeSpec {
	env: Record<string, string>;
	cpuMillis: number;
	memoryMebibytes: number;
	ports: DashboardRuntimePort[];
	healthCheck?: DashboardHTTPHealthCheck;
	livenessCheck?: DashboardHTTPHealthCheck;
	restart?: DashboardRestartSpec;
	volumeName?: string;
}

export interface DashboardServiceSpec {
	source?: DashboardSourceSpec;
	runtime: DashboardRuntimeSpec;
	desiredReplicaCount?: number;
	placementRegion?: string;
	rollingStrategy?: DashboardRollingStrategy;
}

export interface DashboardRollingStrategy {
	healthcheckTimeoutSeconds: number;
	drainingSeconds: number;
}

export type DashboardAgentLifecycleState =
	| "AGENT_LIFECYCLE_STATE_ENROLLING"
	| "AGENT_LIFECYCLE_STATE_ACTIVE"
	| "AGENT_LIFECYCLE_STATE_CORDONED"
	| "AGENT_LIFECYCLE_STATE_DRAINING"
	| "AGENT_LIFECYCLE_STATE_UNAVAILABLE"
	| "AGENT_LIFECYCLE_STATE_RETIRED"
	| "AGENT_LIFECYCLE_STATE_UNSPECIFIED";

export interface DashboardFleetAgent {
	id: string;
	name: string;
	lifecycleState: DashboardAgentLifecycleState;
	region: string;
	zone: string;
	failureDomain: string;
	healthy: boolean;
	lastSeenAt?: Date;
	cpuMillisCapacity: number;
	memoryMebibytesCapacity: number;
	reservedCpuMillis: number;
	reservedMemoryMebibytes: number;
	schedulableCpuMillis: number;
	schedulableMemoryMebibytes: number;
	allocatedCpuMillis: number;
	allocatedMemoryMebibytes: number;
	headroomCpuMillis: number;
	headroomMemoryMebibytes: number;
	allocationCount: number;
	runtimeCapabilities: string[];
	softwareVersion: string;
	versionSkewWarning?: string;
	maintenanceMessage?: string;
	credentialRevokedAt?: Date;
}

export interface DashboardFleet {
	agents: DashboardFleetAgent[];
	capacity: {
		nodeCount: number;
		schedulableNodeCount: number;
		schedulableCpuMillis: number;
		schedulableMemoryMebibytes: number;
		allocatedCpuMillis: number;
		allocatedMemoryMebibytes: number;
		headroomCpuMillis: number;
		headroomMemoryMebibytes: number;
	};
	versionWarning?: string;
}

export interface FleetAgentInput {
	agentId: string;
	name: string;
	region: string;
	zone: string;
	failureDomain: string;
	reservedCpuMillis: number;
	reservedMemoryMebibytes: number;
}

export interface DashboardAgentEnrollment {
	agent: DashboardFleetAgent;
	bootstrapToken: string;
}

export interface DashboardResolvedSourceBinding {
	repositorySelector: string;
	trackedRef: string;
	accessState: RepositoryAccessState;
	buildRecipe?: DashboardBuildRecipe;
}

/**
 * Immutable deploy-by-digest identity. `imageRef` is what runs; a tag only
 * appears as `sourceImageRef`, the user input it was resolved from.
 */
export interface DashboardBuildArtifact {
	id: string;
	kind: "build" | "direct_image";
	imageRef: string;
	sourceImageRef?: string;
	commitSha?: string;
	buildId?: string;
	createdAt?: Date;
}

export interface DashboardBuildStatus {
	buildId: string;
	state: DashboardBuildState;
	commitSha: string;
	imageDigest: string;
	queuedAt?: Date;
	startedAt?: Date;
	finishedAt?: Date;
	failureReason: string;
	commitMessage?: string;
	commitAuthor?: string;
	stages?: Array<DashboardDeploymentStage>;
	builder?: DashboardBuilderKind;
	/** Every claim of the build, including takeovers after worker loss. */
	attemptCount?: number;
	attemptLimit?: number;
	/** Set while a running build has been asked to stop but has not yet. */
	cancelRequestedAt?: Date;
	artifact?: DashboardBuildArtifact;
}

export type DashboardBuildAttemptOutcome =
	| "leased"
	| "succeeded"
	| "failed_terminal"
	| "worker_lost"
	| "cancelled"
	| "timed_out"
	| "superseded"
	| (string & {});

export interface DashboardBuildAttempt {
	attemptNumber: number;
	builderId: string;
	startedAt?: Date;
	finishedAt?: Date;
	outcome: DashboardBuildAttemptOutcome;
	detail: string;
}

export interface DashboardDeploymentStage {
	key: string;
	label: string;
	detail: string;
	state: DashboardDeploymentStageState;
	startedAt?: Date;
	finishedAt?: Date;
}

export interface DashboardSourceRevision {
	commitSha: string;
	observedAt?: Date;
}

export interface DashboardServiceSourceSummary {
	desiredSpec?: DashboardSourceSpec;
	resolvedBinding?: DashboardResolvedSourceBinding;
	latestRevision?: DashboardSourceRevision;
}

export interface DashboardServicePosition {
	x: number;
	y: number;
}

export type DashboardUnappliedChangeAction =
	| "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD"
	| "SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE"
	| "SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE"
	| "SERVICE_UNAPPLIED_CHANGE_ACTION_UNSPECIFIED";

export interface DashboardUnappliedChange {
	id: string;
	section: string;
	field: string;
	path: string;
	action: DashboardUnappliedChangeAction;
	currentValue: string;
	newValue: string;
}

export interface DashboardServiceRecord {
	id: string;
	environmentId: string;
	/** Derived dashboard ancestry; never sent as service authority. */
	projectId?: string;
	name: string;
	internalHostname?: string;
	spec?: DashboardServiceSpec;
	specRevision?: number;
	allocatedAgentId?: string;
	createdAt?: Date;
	updatedAt?: Date;
	rolloutGeneration?: number;
	sourceSummary?: DashboardServiceSourceSummary;
	lastSuccessfulCommitSha?: string;
	resolvedImage?: string;
	latestBuild?: DashboardBuildStatus;
	latestDeployment?: DashboardDeploymentStatus;
	pendingChanges?: boolean;
	unappliedChangeCount?: number;
	unappliedChanges?: Array<DashboardUnappliedChange>;
	layoutPosition?: DashboardServicePosition;
	desiredReplicaCount?: number;
	readyReplicaCount?: number;
	placementMessage?: string;
	deletion?: DashboardDeletionState;
}

export interface DashboardRestartObservation {
	restartCount: number;
	crashLoop: boolean;
	lastCause: string;
	message: string;
	lastExitCode: number;
	lastSignal: number;
	awaitingRestart: boolean;
	windowStartedAt?: Date;
	lastRestartAt?: Date;
	nextRestartAt?: Date;
	startedAt?: Date;
}

export interface DashboardAllocationStatus {
	allocationId: string;
	serviceId: string;
	agentId: string;
	desiredSpecRevision: number;
	appliedSpecRevision: number;
	phase: string;
	message: string;
	allocationIpv4: string;
	allocationIpv6: string;
	healthy: boolean;
	updatedAt?: Date;
	desiredRolloutGeneration: number;
	appliedRolloutGeneration: number;
	healthyIpv4Ports: number[];
	healthyIpv6Ports: number[];
	operatorRestartNonce?: number;
	rolloutState?: string;
	drainStartedAt?: Date;
	drainDeadline?: Date;
	restart?: DashboardRestartObservation;
}

export interface DashboardServiceStatus {
	service: DashboardServiceRecord;
	allocation?: DashboardAllocationStatus;
	allocations?: Array<DashboardAllocationStatus>;
}

export interface DashboardIndexedServices {
	index: number;
	notModified: boolean;
	services?: Array<DashboardServiceRecord>;
}

export interface DashboardIndexedServiceStatus {
	index: number;
	notModified: boolean;
	status?: DashboardServiceStatus;
}

export interface DashboardServiceLogLine {
	/** Stable identity for ordering and deduplication across pages. */
	lineId?: string;
	observedAt?: Date;
	allocationId: string;
	agentId: string;
	stream: string;
	rolloutGeneration: number;
	sequence: number;
	line: string;
	logType?: DashboardServiceLogType;
	buildId?: string;
	stage?: string;
	/** Platform event name (e.g. deploy.started); empty for customer output. */
	event?: string;
	attributes?: Record<string, string>;
	/** The line was cut at the 64 KiB line limit. */
	truncated?: boolean;
}

/** A window where lines were dropped instead of delivered. */
export interface DashboardServiceLogGap {
	allocationId: string;
	buildId: string;
	logType?: DashboardServiceLogType;
	stream: string;
	droppedCount: number;
	reason: string;
	windowStart?: Date;
	windowEnd?: Date;
}

/**
 * One page of the log timeline. Lines and gaps paginate independently and
 * only move toward older entries.
 */
export interface DashboardServiceLogPage {
	lines: Array<DashboardServiceLogLine>;
	nextPageToken?: string;
	gaps: Array<DashboardServiceLogGap>;
	nextGapPageToken?: string;
}

/** Masked existence record for a sealed secret: never its value. */
export interface DashboardServiceSecret {
	name: string;
	version: number;
	updatedAt?: Date;
}

export interface DashboardDeploymentRecord {
	id: string;
	rolloutGeneration: number;
	specRevision?: number;
	createdAt?: Date;
	repositorySelector?: string;
	trackedRef?: string;
	build?: DashboardBuildStatus;
	allocation?: DashboardAllocationStatus;
	isCurrent: boolean;
	status?: DashboardDeploymentStatus;
	stages?: Array<DashboardDeploymentStage>;
	imageDigest?: string;
	actions?: Array<DashboardDeploymentActionRecord>;
	variableVersions?: Record<string, number>;
	/** Sealed secret name to the version pinned at deploy time. */
	sealedVersions?: Record<string, number>;
	artifact?: DashboardBuildArtifact;
}

export interface CreateServiceFastResult {
	project: DashboardProject;
	environment: DashboardEnvironment;
	service: DashboardServiceRecord;
	serviceStatus: DashboardServiceStatus | null;
	onboarding: DashboardOnboardingDraft;
}

export type DashboardDomainOwnershipState =
	| "DOMAIN_OWNERSHIP_STATE_UNSPECIFIED"
	| "DOMAIN_OWNERSHIP_STATE_VERIFIED"
	| "DOMAIN_OWNERSHIP_STATE_UNVERIFIED";

export interface DashboardDomainBinding {
	hostname: string;
	/** Derived dashboard ancestry; never sent as domain authority. */
	projectId?: string;
	serviceId: string;
	targetPort: number;
	platformGenerated: boolean;
	ownershipState: DashboardDomainOwnershipState;
	ownershipMessage?: string;
	deletion?: DashboardDeletionState;
}

export interface DashboardHomeState {
	user: DashboardUser;
	canManageFleet?: boolean;
	githubAccount?: DashboardGitHubAccount;
	onboarding: DashboardOnboardingDraft;
	repositories: Array<GitHubUserRepository>;
	githubLoginURL?: string;
	githubInstallURL?: string;
	publicBaseURL: string;
	localIngressBaseURL?: string;
	ingressTargetHost: string;
	localDomainSuffix?: string;
	project?: DashboardProject;
	/** Every live project, for switching. */
	projects: Array<DashboardProject>;
	environments: Array<DashboardEnvironment>;
	environment?: DashboardEnvironment;
	services: Array<DashboardServiceRecord>;
	servicesRevision: number;
	selectedServiceId: string | null;
	domainBindings: Array<DashboardDomainBinding>;
	controlPlaneReachable: boolean;
	controlPlaneError?: string;
}

export interface DevLoginIdentity {
	id: string;
	email: string;
}

export interface GitHubAppUserAuthConfig {
	appId: string;
	clientId: string;
	clientSecret: string;
	authorizationBaseURL: string;
	apiBaseURL: string;
}

export type DashboardRuntimeProfile = "development" | "production";

export interface DashboardConfig {
	profile: DashboardRuntimeProfile;
	sessionCookieName: string;
	refreshCookieName: string;
	authStateCookieName: string;
	publicBaseURL: string;
	localIngressBaseURL?: string;
	devUsers: Array<DevLoginIdentity>;
	sessionMaxAgeSeconds: number;
	jwtSecret: string;
	/**
	 * The previous session signing secret during a rotation overlap. Tokens
	 * verify against it after the active secret; nothing is signed with it.
	 */
	jwtSecretPrevious?: string;
	githubInstallURL?: string;
	operatorGitHubLogin?: string;
	ingressTargetHost: string;
	localDomainSuffix?: string;
	github?: GitHubAppUserAuthConfig;
}

export interface DashboardSession {
	user: DashboardUser;
}
