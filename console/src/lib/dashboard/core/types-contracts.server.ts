import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";
import type {
	CreateServiceFastResult,
	DashboardAgentEnrollment,
	DashboardAgentLifecycleState,
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDomainBinding,
	DashboardEnvironment,
	DashboardFleet,
	DashboardFleetAgent,
	DashboardGitHubAccount,
	DashboardHomeState,
	DashboardIndexedServiceStatus,
	DashboardIndexedServices,
	DashboardOnboardingDraft,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardRestartSpec,
	DashboardRollingStrategy,
	DashboardServiceLogLine,
	DashboardServiceLogType,
	DashboardServicePosition,
	DashboardServiceRecord,
	DashboardServiceSpec,
	DashboardServiceStatus,
	DashboardUser,
	DevLoginIdentity,
	FleetAgentInput,
	GitHubUserRepository,
	StoredDashboardGitHubAccount,
} from "./types-models.server";

export interface GitHubAccountLoginInput {
	userID?: string;
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
	listFleet(user: DashboardUser): Promise<DashboardFleet>;
	createFleetAgent(
		user: DashboardUser,
		input: FleetAgentInput,
	): Promise<DashboardAgentEnrollment>;
	updateFleetAgent(
		user: DashboardUser,
		input: FleetAgentInput,
	): Promise<DashboardFleetAgent>;
	setFleetAgentLifecycle(
		user: DashboardUser,
		input: { agentId: string; lifecycleState: DashboardAgentLifecycleState },
	): Promise<DashboardFleetAgent>;
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
	applyDeploymentAction(
		user: DashboardUser,
		input: {
			serviceId: string;
			deploymentId: string;
			action: DashboardDeploymentAction;
			idempotencyKey: string;
			allocationId?: string;
		},
	): Promise<DashboardServiceStatus>;
	scaleService(
		user: DashboardUser,
		input: {
			serviceId: string;
			desiredReplicaCount: number;
		},
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
	restart?: DashboardRestartSpec;
	desiredReplicaCount?: number;
	placementRegion?: string;
	rollingStrategy?: DashboardRollingStrategy;
}

export interface DashboardService {
	loadFleetFromSession(): Promise<DashboardFleet>;
	createFleetAgentFromSession(
		input: FleetAgentInput,
	): Promise<DashboardAgentEnrollment>;
	updateFleetAgentFromSession(
		input: FleetAgentInput,
	): Promise<DashboardFleetAgent>;
	setFleetAgentLifecycleFromSession(input: {
		agentId: string;
		lifecycleState: DashboardAgentLifecycleState;
	}): Promise<DashboardFleetAgent>;
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
	applyDeploymentActionFromSession(input: {
		serviceId: string;
		deploymentId: string;
		action: DashboardDeploymentAction;
		idempotencyKey: string;
		allocationId?: string;
	}): Promise<DashboardServiceStatus>;
	scaleServiceFromSession(input: {
		serviceId: string;
		desiredReplicaCount: number;
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
