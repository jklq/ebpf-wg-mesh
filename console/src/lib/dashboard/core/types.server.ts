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
	| "unspecified";

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
}

export interface DashboardServiceSpec {
	source?: DashboardSourceSpec;
	runtime: DashboardRuntimeSpec;
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
}

export interface DashboardServiceStatus {
	service: DashboardServiceRecord;
	allocation?: DashboardAllocationStatus;
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
	ingressTargetHost: string;
	localDomainSuffix?: string;
	github?: GitHubAppUserAuthConfig;
}

export interface DashboardSession {
	user: DashboardUser;
}

export type AuthConflictCode =
	| "missing_verified_email"
	| "email_linked_to_other_github"
	| "ambiguous_existing_user"
	| "invalid_signin_state"
	| "github_auth_unavailable"
	| "dev_auth_unavailable"
	| "invalid_dev_login";

class DashboardTaggedError extends Error {
	readonly _tag: string;

	constructor(tag: string, message: string) {
		super(message);
		this.name = tag;
		this._tag = tag;
		Object.setPrototypeOf(this, new.target.prototype);
	}
}

export class AuthConflictError extends DashboardTaggedError {
	readonly code: AuthConflictCode;

	constructor(input: { code: AuthConflictCode; message: string }) {
		super("AuthConflictError", input.message);
		this.code = input.code;
	}
}

export class AuthenticationRequiredError extends DashboardTaggedError {
	constructor(input: { message: string }) {
		super("AuthenticationRequiredError", input.message);
	}
}

export class DashboardValidationError extends DashboardTaggedError {
	constructor(input: { message: string }) {
		super("DashboardValidationError", input.message);
	}
}

export class DashboardConfigError extends DashboardTaggedError {
	constructor(input: { message: string }) {
		super("DashboardConfigError", input.message);
	}
}

export class DatabaseError extends DashboardTaggedError {
	readonly operation: string;
	readonly cause: unknown;

	constructor(input: { operation: string; message: string; cause: unknown }) {
		super("DatabaseError", input.message);
		this.operation = input.operation;
		this.cause = input.cause;
	}
}

export class GitHubApiError extends DashboardTaggedError {
	readonly operation: string;
	readonly cause: unknown;
	readonly status?: number;

	constructor(input: {
		operation: string;
		message: string;
		cause: unknown;
		status?: number;
	}) {
		super("GitHubApiError", input.message);
		this.operation = input.operation;
		this.cause = input.cause;
		this.status = input.status;
	}
}

export class PlatformGatewayError extends DashboardTaggedError {
	readonly operation: string;
	readonly cause: unknown;
	readonly grpcCode?: number;

	constructor(input: {
		operation: string;
		message: string;
		cause: unknown;
		grpcCode?: number;
	}) {
		super("PlatformGatewayError", input.message);
		this.operation = input.operation;
		this.cause = input.cause;
		this.grpcCode = input.grpcCode;
	}
}

export interface GitHubAccountLoginInput {
	providerSubject: string;
	login: string;
	primaryEmail: string;
	accessToken: string;
	tokenType: string;
	scope: string;
	accessTokenExpiresAt?: Date;
	refreshToken?: string;
	refreshTokenExpiresAt?: Date;
}

export interface GitHubAccountLoginResult {
	user: DashboardUser;
	disposition: "login" | "link" | "signup";
}

export interface DashboardStore {
	ensureInitialized(): Promise<void>;
	upsertDevUser(userID: string, email: string): Promise<DashboardUser>;
	completeGitHubLogin(
		input: GitHubAccountLoginInput,
	): Promise<GitHubAccountLoginResult>;
	getGitHubAccount(
		userID: string,
	): Promise<StoredDashboardGitHubAccount | null>;
	tryAcquireGitHubTokenRefresh(input: {
		userID: string;
		expectedTokenVersion: number;
		leaseID: string;
		now: Date;
		leaseExpiresAt: Date;
	}): Promise<boolean>;
	completeGitHubTokenRefresh(input: {
		userID: string;
		providerSubject: string;
		expectedTokenVersion: number;
		leaseID: string;
		token: GitHubAppUserToken;
		fallbackRefreshToken: string;
		fallbackRefreshTokenExpiresAt?: Date;
	}): Promise<StoredDashboardGitHubAccount | null>;
	releaseGitHubTokenRefresh(input: {
		userID: string;
		expectedTokenVersion: number;
		leaseID: string;
	}): Promise<void>;
	getOnboardingDraft(userID: string): Promise<DashboardOnboardingDraft>;
	saveOnboardingDraft(
		userID: string,
		draft: DashboardOnboardingDraft,
	): Promise<DashboardOnboardingDraft>;
	listServicePositions(
		userID: string,
		environmentId: string,
	): Promise<Record<string, DashboardServicePosition>>;
	saveServicePosition(
		userID: string,
		input: {
			environmentId: string;
			serviceId: string;
			position: DashboardServicePosition;
		},
	): Promise<DashboardServicePosition>;
	createRefreshSession(
		sessionId: string,
		userID: string,
		expiresAt: Date,
	): Promise<void>;
	deleteRefreshSession(sessionId: string): Promise<void>;
	rotateRefreshSession(input: {
		sessionId: string;
		userID: string;
		now: Date;
		nextSessionId: string;
		expiresAt: Date;
	}): Promise<DashboardUser | null>;
}

export interface PlatformGateway {
	listProjects(user: DashboardUser): Promise<Array<DashboardProject>>;
	createProject(user: DashboardUser, name: string): Promise<DashboardProject>;
	listEnvironments(
		user: DashboardUser,
		projectId: string,
	): Promise<Array<DashboardEnvironment>>;
	getEnvironment(
		user: DashboardUser,
		environmentId: string,
	): Promise<DashboardEnvironment>;
	createEnvironment(
		user: DashboardUser,
		input: { projectId: string; name: string },
	): Promise<DashboardEnvironment>;
	duplicateEnvironment(
		user: DashboardUser,
		input: {
			sourceEnvironmentId: string;
			name: string;
			copyVariables: boolean;
		},
	): Promise<DashboardEnvironment>;
	renameEnvironment(
		user: DashboardUser,
		input: { environmentId: string; name: string },
	): Promise<DashboardEnvironment>;
	deleteEnvironment(user: DashboardUser, environmentId: string): Promise<void>;
	deployEnvironment(
		user: DashboardUser,
		environmentId: string,
	): Promise<Array<DashboardServiceStatus>>;
	listServices(
		user: DashboardUser,
		environmentId: string,
	): Promise<Array<DashboardServiceRecord>>;
	waitForServices(
		user: DashboardUser,
		input: {
			environmentId: string;
			waitIndex: number;
			waitTimeoutSeconds: number;
		},
	): Promise<DashboardIndexedServices>;
	inspectRepositorySource(
		user: DashboardUser,
		input: {
			projectId: string;
			provider: string;
			repositorySelector: string;
			githubUserAccessToken: string;
		},
	): Promise<DashboardRepositoryInspection>;
	linkGitHubRepository(
		user: DashboardUser,
		input: {
			projectId: string;
			repositorySelector: string;
			githubUserAccessToken: string;
		},
	): Promise<DashboardRepositoryInspection>;
	createService(
		user: DashboardUser,
		input: {
			environmentId: string;
			name: string;
			spec: DashboardServiceSpec;
		},
	): Promise<DashboardServiceRecord>;
	updateService(
		user: DashboardUser,
		input: {
			serviceId: string;
			name?: string;
			spec: DashboardServiceSpec;
		},
	): Promise<DashboardServiceRecord>;
	redeployService(
		user: DashboardUser,
		input: { serviceId: string },
	): Promise<DashboardServiceStatus>;
	discardServiceChanges(
		user: DashboardUser,
		input: {
			serviceId: string;
			changeIds?: Array<string>;
			discardAll?: boolean;
		},
	): Promise<DashboardServiceRecord>;
	deleteService(
		user: DashboardUser,
		input: { serviceId: string },
	): Promise<void>;
	getService(
		user: DashboardUser,
		input: { serviceId: string },
	): Promise<DashboardServiceRecord>;
	getServiceStatus(
		user: DashboardUser,
		input: { serviceId: string },
	): Promise<DashboardServiceStatus>;
	waitForServiceStatus(
		user: DashboardUser,
		input: {
			serviceId: string;
			waitIndex: number;
			waitTimeoutSeconds: number;
		},
	): Promise<DashboardIndexedServiceStatus>;
	listServiceLogs(
		user: DashboardUser,
		input: {
			serviceId: string;
			allocationId?: string;
			limit?: number;
			logType?: DashboardServiceLogType;
			buildId?: string;
			search?: string;
			startTime?: Date;
			endTime?: Date;
		},
	): Promise<Array<DashboardServiceLogLine>>;
	listServiceDeployments(
		user: DashboardUser,
		input: { serviceId: string; limit?: number },
	): Promise<Array<DashboardDeploymentRecord>>;
	listDomainBindings(
		user: DashboardUser,
		input: { serviceId: string },
	): Promise<Array<DashboardDomainBinding>>;
	generateDomainBinding(
		user: DashboardUser,
		input: { serviceId: string; targetPort: number },
	): Promise<DashboardDomainBinding>;
	createDomainBinding(
		user: DashboardUser,
		input: {
			serviceId: string;
			hostname: string;
			targetPort: number;
		},
	): Promise<DashboardDomainBinding>;
	updateDomainBinding(
		user: DashboardUser,
		input: {
			hostname: string;
			serviceId: string;
			targetPort: number;
		},
	): Promise<DashboardDomainBinding>;
	deleteDomainBinding(
		user: DashboardUser,
		input: { hostname: string },
	): Promise<void>;
}

export interface GitHubAppUserToken {
	accessToken: string;
	tokenType: string;
	scope: string;
	accessTokenExpiresAt?: Date;
	refreshToken?: string;
	refreshTokenExpiresAt?: Date;
}

export interface GitHubAppUserIdentity {
	providerSubject: string;
	login: string;
	primaryEmail: string;
}

export interface GitHubAppUserClient {
	buildAuthorizationURL(input: { redirectURI: string; state: string }): string;
	exchangeCode(input: {
		code: string;
		redirectURI: string;
	}): Promise<GitHubAppUserToken>;
	refreshToken(refreshToken: string): Promise<GitHubAppUserToken>;
	fetchIdentity(accessToken: string): Promise<GitHubAppUserIdentity>;
	listRepositories(accessToken: string): Promise<Array<GitHubUserRepository>>;
}

export interface SessionCookieOptions {
	httpOnly: boolean;
	path: string;
	sameSite: "lax";
	secure: boolean;
	expires: Date;
}

export interface SessionCookies {
	get(name: string): string | undefined;
	set(name: string, value: string, options: SessionCookieOptions): void;
	delete(name: string, options: { path: string }): void;
}

export interface DashboardDependencies {
	store: DashboardStore;
	platform: PlatformGateway;
	cookies: SessionCookies;
	github?: GitHubAppUserClient;
	now?: () => Date;
	randomUUID?: () => string;
}

export interface UpdateServiceInput {
	serviceId: string;
	serviceName?: string;
	runtimeEnv?: Record<string, string>;
	cpuMillis?: number;
	memoryMebibytes?: number;
	repositorySelector?: string;
	trackedRef?: string;
	dockerfilePath?: string;
	contextDir?: string;
}

export interface DashboardService {
	listDevLogins(): Array<DevLoginIdentity>;
	isGitHubLoginEnabled(): boolean;
	getPublicBaseURL(): string;
	beginGitHubLogin(input: { redirectTo?: string }): Promise<string>;
	completeAuthCallback(input: {
		code?: string;
		state?: string;
		userId?: string;
		email?: string;
		redirectTo?: string;
	}): Promise<string>;
	loadDashboardHome(environmentId?: string): Promise<DashboardHomeState | null>;
	createEnvironmentFromSession(input: {
		projectId: string;
		name: string;
	}): Promise<DashboardEnvironment>;
	duplicateEnvironmentFromSession(input: {
		sourceEnvironmentId: string;
		name: string;
		copyVariables: boolean;
	}): Promise<DashboardEnvironment>;
	renameEnvironmentFromSession(input: {
		environmentId: string;
		name: string;
	}): Promise<DashboardEnvironment>;
	deleteEnvironmentFromSession(environmentId: string): Promise<void>;
	deployEnvironmentFromSession(
		environmentId: string,
	): Promise<Array<DashboardServiceStatus>>;
	loadGitHubCatalogFromSession(): Promise<{
		githubAccount?: DashboardGitHubAccount;
		repositories: Array<GitHubUserRepository>;
	}>;
	inspectRepositorySourceFromSession(input: {
		repositorySelector: string;
	}): Promise<DashboardRepositoryInspection | undefined>;
	createProjectFromSession(name: string): Promise<DashboardProject>;
	inspectRepositoryFromSession(input: {
		repositorySelector: string;
	}): Promise<DashboardOnboardingDraft>;
	confirmRepositoryFromSession(input: {
		repositorySelector: string;
		serviceName?: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		cpuMillis?: number;
		memoryMebibytes?: number;
	}): Promise<DashboardOnboardingDraft>;
	createServiceFastFromSession(input: {
		repositorySelector: string;
		serviceName?: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		cpuMillis?: number;
		memoryMebibytes?: number;
	}): Promise<CreateServiceFastResult>;
	saveHostnameFromSession(hostname: string): Promise<DashboardOnboardingDraft>;
	publishDomainFromSession(): Promise<DashboardDomainBinding>;
	clearSession(): Promise<void>;
	refreshSession(): Promise<void>;
	getServiceStatusFromSession(input: {
		serviceId: string;
	}): Promise<DashboardServiceStatus>;
	listEnvironmentServicesFromSession(input: {
		environmentId: string;
	}): Promise<Array<DashboardServiceRecord>>;
	waitForEnvironmentServicesFromSession(input: {
		environmentId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	}): Promise<DashboardIndexedServices>;
	waitForProjectServicesFromSession(input: {
		projectId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	}): Promise<DashboardIndexedServices>;
	waitForServiceStatusFromSession(input: {
		serviceId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	}): Promise<DashboardIndexedServiceStatus>;
	listServiceLogsFromSession(input: {
		serviceId: string;
		allocationId?: string;
		limit?: number;
		logType?: DashboardServiceLogType;
		buildId?: string;
		search?: string;
		startTime?: Date;
		endTime?: Date;
	}): Promise<Array<DashboardServiceLogLine>>;
	listServiceDeploymentsFromSession(input: {
		serviceId: string;
		limit?: number;
	}): Promise<Array<DashboardDeploymentRecord>>;
	updateServiceFromSession(
		input: UpdateServiceInput,
	): Promise<DashboardServiceRecord>;
	redeployServiceFromSession(input: {
		serviceId: string;
	}): Promise<DashboardServiceStatus>;
	discardServiceChangesFromSession(input: {
		serviceId: string;
		changeIds?: Array<string>;
		discardAll?: boolean;
	}): Promise<DashboardServiceRecord>;
	deleteServiceFromSession(input: { serviceId: string }): Promise<void>;
	saveServicePositionFromSession(input: {
		environmentId: string;
		serviceId: string;
		position: DashboardServicePosition;
	}): Promise<DashboardServicePosition>;
	listDomainBindingsFromSession(input: {
		serviceId: string;
	}): Promise<Array<DashboardDomainBinding>>;
	generateDomainBindingFromSession(input: {
		serviceId: string;
		targetPort: string | number | undefined;
	}): Promise<DashboardDomainBinding>;
	createDomainBindingFromSession(input: {
		serviceId: string;
		hostname: string;
		targetPort: string | number | undefined;
	}): Promise<DashboardDomainBinding>;
	updateDomainBindingFromSession(input: {
		serviceId: string;
		hostname: string;
		targetPort: string | number | undefined;
	}): Promise<DashboardDomainBinding>;
	deleteDomainBindingFromSession(input: { hostname: string }): Promise<void>;
	checkDomainDNSFromSession(
		hostname: string,
	): Promise<DomainVerificationResult | undefined>;
}
