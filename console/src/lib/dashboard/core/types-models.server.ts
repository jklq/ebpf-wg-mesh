import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";

export interface DashboardUser {
	id: string;
	email: string;
}

export type DashboardProjectKind = "user" | "managed";

export interface DashboardProject {
	id: string;
	name: string;
	kind: DashboardProjectKind;
	systemKey?: string;
}

export interface DashboardEnvironment {
	id: string;
	projectId: string;
	name: string;
	kind: "persistent";
	isProduction: boolean;
	copiedFromEnvironmentId?: string;
	createdAt?: Date;
	updatedAt?: Date;
}

export type DashboardOnboardingStep =
	| "account"
	| "repository"
	| "build"
	| "domain";

export type RepositoryAccessState =
	| "available"
	| "installation_required"
	| "access_revoked"
	| "repository_deleted"
	| "unspecified";

export type DashboardBuildState =
	| "queued"
	| "running"
	| "succeeded"
	| "failed"
	| "superseded"
	| "cancelled"
	| "unspecified";

export type DashboardDeploymentAction =
	| "restart"
	| "exact_redeploy"
	| "rollback"
	| "cancel"
	| "remove"
	| "retry";

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
	| "pending"
	| "running"
	| "succeeded"
	| "failed"
	| "skipped"
	| "unspecified";

export type DashboardDeploymentState =
	| "staged"
	| "queued_build"
	| "building"
	| "scheduling"
	| "image_pull"
	| "starting"
	| "readiness"
	| "active"
	| "draining"
	| "completed"
	| "failed"
	| "cancelled"
	| "crashed"
	| "removed"
	| "superseded"
	| "unspecified";

export type DashboardDeploymentCauseKind =
	| "user"
	| "system"
	| "agent"
	| "builder"
	| "webhook"
	| "unspecified";

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
}

export type DashboardServiceLogType =
	| "runtime"
	| "build"
	| "deploy"
	| "http"
	| "network"
	| "unspecified";

export interface DashboardBuildRecipe {
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
	currentStep: DashboardOnboardingStep;
	projectId: string;
	environmentId: string;
	serviceId: string;
	repositorySelector: string;
	trackedRef: string;
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
	recommendedPorts: number[];
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

export type DashboardRestartPolicy = "always" | "on-failure" | "never";

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
	| "enrolling"
	| "active"
	| "cordoned"
	| "draining"
	| "unavailable"
	| "retired"
	| "unspecified";

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
}

export interface DashboardDeploymentStage {
	key: string;
	label: string;
	detail: string;
	state: DashboardDeploymentStageState;
	startedAt?: Date;
	finishedAt?: Date;
}

export interface DashboardServiceSourceSummary {
	desiredSpec?: DashboardSourceSpec;
	resolvedBinding?: DashboardResolvedSourceBinding;
}

export interface DashboardServicePosition {
	x: number;
	y: number;
}

export type DashboardUnappliedChangeAction =
	| "add"
	| "update"
	| "remove"
	| "unspecified";

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
}

export interface DashboardAllocationStatus {
	allocationId: string;
	serviceId: string;
	agentId: string;
	desiredSpecRevision: number;
	appliedSpecRevision: number;
	phase: string;
	message: string;
	allocationIp: string;
	healthy: boolean;
	updatedAt?: Date;
	desiredRolloutGeneration: number;
	appliedRolloutGeneration: number;
	healthyPorts: number[];
	operatorRestartNonce?: number;
	rolloutState?: string;
	drainStartedAt?: Date;
	drainDeadline?: Date;
	restart?: {
		restartCount: number;
		crashLoop: boolean;
		lastCause: string;
		message: string;
	};
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
}

export interface CreateServiceFastResult {
	project: DashboardProject;
	environment: DashboardEnvironment;
	service: DashboardServiceRecord;
	serviceStatus: DashboardServiceStatus | null;
	onboarding: DashboardOnboardingDraft;
}

export type DashboardDomainOwnershipState =
	| "unspecified"
	| "verified"
	| "unverified";

export interface DashboardDomainBinding {
	hostname: string;
	/** Derived dashboard ancestry; never sent as domain authority. */
	projectId?: string;
	serviceId: string;
	targetPort: number;
	platformGenerated: boolean;
	ownershipState: DashboardDomainOwnershipState;
	ownershipMessage?: string;
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
	environments: Array<DashboardEnvironment>;
	environment?: DashboardEnvironment;
	services: Array<DashboardServiceRecord>;
	service?: DashboardServiceRecord;
	serviceStatus?: DashboardServiceStatus;
	repositoryInspection?: DashboardRepositoryInspection;
	domainVerification?: DomainVerificationResult;
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
	githubInstallURL?: string;
	operatorGitHubLogin?: string;
	ingressTargetHost: string;
	localDomainSuffix?: string;
	github?: GitHubAppUserAuthConfig;
}

export interface DashboardSession {
	user: DashboardUser;
}
