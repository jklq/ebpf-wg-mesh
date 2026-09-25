import type {
	DashboardAgentEnrollment,
	DashboardAgentLifecycleState,
	DashboardBuildAttempt,
	DashboardBuilderKind,
	DashboardDeletionPreview,
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDomainBinding,
	DashboardEnvironment,
	DashboardFleet,
	DashboardFleetAgent,
	DashboardIndexedServiceStatus,
	DashboardIndexedServices,
	DashboardOnboardingDraft,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardRestartSpec,
	DashboardRollingStrategy,
	DashboardServiceLogPage,
	DashboardServiceLogType,
	DashboardServicePosition,
	DashboardServiceRecord,
	DashboardServiceSecret,
	DashboardServiceSpec,
	DashboardServiceStatus,
	DashboardUser,
	DashboardVolume,
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
	listProjects(
		user: DashboardUser,
		options?: { includeDeleted?: boolean },
	): Promise<Array<DashboardProject>>;
	getProject(user: DashboardUser, projectId: string): Promise<DashboardProject>;
	createProject(user: DashboardUser, name: string): Promise<DashboardProject>;
	updateProjectLogRetention(
		user: DashboardUser,
		input: { projectId: string; logRetentionDays: number },
	): Promise<DashboardProject>;
	previewProjectDeletion(
		user: DashboardUser,
		projectId: string,
	): Promise<DashboardDeletionPreview>;
	deleteProject(
		user: DashboardUser,
		input: { projectId: string; confirmationName: string },
	): Promise<void>;
	restoreProject(
		user: DashboardUser,
		projectId: string,
	): Promise<DashboardProject>;
	listEnvironments(
		user: DashboardUser,
		projectId: string,
		options?: { includeDeleted?: boolean },
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
	updateEnvironmentAutoDeploy(
		user: DashboardUser,
		input: { environmentId: string; autoDeploy: boolean },
	): Promise<DashboardEnvironment>;
	previewEnvironmentDeletion(
		user: DashboardUser,
		environmentId: string,
	): Promise<DashboardDeletionPreview>;
	deleteEnvironment(
		user: DashboardUser,
		input: { environmentId: string; confirmationName?: string },
	): Promise<void>;
	restoreEnvironment(
		user: DashboardUser,
		environmentId: string,
	): Promise<DashboardEnvironment>;
	releaseEnvironment(
		user: DashboardUser,
		environmentId: string,
	): Promise<Array<DashboardServiceStatus>>;
	listServices(
		user: DashboardUser,
		environmentId: string,
		options?: { includeDeleted?: boolean },
	): Promise<DashboardIndexedServices>;
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
	restoreService(
		user: DashboardUser,
		serviceId: string,
	): Promise<DashboardServiceRecord>;
	listServiceSecrets(
		user: DashboardUser,
		serviceId: string,
	): Promise<Array<DashboardServiceSecret>>;
	sealServiceSecret(
		user: DashboardUser,
		input: { serviceId: string; name: string; value: string },
	): Promise<DashboardServiceSecret>;
	deleteServiceSecret(
		user: DashboardUser,
		input: { serviceId: string; name: string },
	): Promise<void>;
	listVolumes(
		user: DashboardUser,
		environmentId: string,
		options?: { includeDeleted?: boolean },
	): Promise<Array<DashboardVolume>>;
	previewVolumeDeletion(
		user: DashboardUser,
		volumeId: string,
	): Promise<DashboardDeletionPreview>;
	deleteVolume(
		user: DashboardUser,
		input: { volumeId: string; confirmationName: string },
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
			pageToken?: string;
			gapPageToken?: string;
		},
	): Promise<DashboardServiceLogPage>;
	listServiceDeployments(
		user: DashboardUser,
		input: { serviceId: string; limit?: number },
	): Promise<Array<DashboardDeploymentRecord>>;
	listBuildAttempts(
		user: DashboardUser,
		input: { serviceId: string; buildId: string },
	): Promise<Array<DashboardBuildAttempt>>;
	listDomainBindings(
		user: DashboardUser,
		input: { serviceId: string; includeDeleted?: boolean },
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
	restoreDomainBinding(
		user: DashboardUser,
		hostname: string,
	): Promise<DashboardDomainBinding>;
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
	builder?: DashboardBuilderKind;
	dockerfilePath?: string;
	contextDir?: string;
	restart?: DashboardRestartSpec;
	desiredReplicaCount?: number;
	placementRegion?: string;
	rollingStrategy?: DashboardRollingStrategy;
}
