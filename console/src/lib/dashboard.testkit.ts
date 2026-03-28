import {
	AuthConflictError,
	createDashboardService,
	type DashboardConfig,
	type DashboardDomainBinding,
	type DashboardGitHubAccount,
	type DashboardOnboardingDraft,
	type DashboardProject,
	type DashboardRepositoryInspection,
	type DashboardServiceRecord,
	type DashboardServiceStatus,
	type DashboardSourceSpec,
	type DashboardService,
	type DashboardStore,
	type DashboardUser,
	type GitHubUserRepository,
	type GitHubAppUserClient,
	type PlatformGateway,
	type SessionCookies,
} from "#/lib/dashboard-core.server";

interface RefreshSessionRecord {
	id: string;
	userID: string;
	expiresAt: Date;
}

interface FakeGitHubAccount {
	user: DashboardUser;
	providerSubject: string;
	email: string;
	login: string;
	accessToken: string;
	tokenType: string;
	scope: string;
	accessTokenExpiresAt?: Date;
	refreshToken?: string;
	refreshTokenExpiresAt?: Date;
}

export interface DashboardTestHarness {
	config: DashboardConfig;
	service: DashboardService;
	store: DashboardStore;
	platform: FakePlatformGateway;
	cookies: FakeSessionCookies;
	github: FakeGitHubAppUserClient;
	users: Map<string, DashboardUser>;
	storeEnsureInitializedCalls: Array<number>;
}

export interface FakePlatformGateway extends PlatformGateway {
	ensurePrincipalCalls: Array<DashboardUser>;
	listProjectsCalls: Array<DashboardUser>;
	createProjectCalls: Array<{ user: DashboardUser; name: string }>;
	listServicesCalls: Array<{ user: DashboardUser; projectId: string }>;
	inspectRepositorySourceCalls: Array<{
		user: DashboardUser;
		provider: string;
		repositorySelector: string;
	}>;
	createServiceCalls: Array<{
		user: DashboardUser;
		projectId: string;
		name: string;
		source: DashboardSourceSpec;
	}>;
	updateServiceCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
		source: DashboardSourceSpec;
	}>;
	getServiceCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
	}>;
	getServiceStatusCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
	}>;
	listDomainBindingsCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
	}>;
	createDomainBindingCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
		hostname: string;
	}>;
	projects: Array<DashboardProject>;
	services: Array<DashboardServiceRecord>;
	serviceStatuses: Map<string, DashboardServiceStatus>;
	domainBindings: Array<DashboardDomainBinding>;
	nextRepositoryInspection?: DashboardRepositoryInspection;
	errors: {
		ensurePrincipal?: Error;
		listProjects?: Error;
		createProject?: Error;
		listServices?: Error;
		inspectRepositorySource?: Error;
		createService?: Error;
		updateService?: Error;
		getService?: Error;
		getServiceStatus?: Error;
		listDomainBindings?: Error;
		createDomainBinding?: Error;
	};
}

export interface FakeGitHubAppUserClient extends GitHubAppUserClient {
	authorizationURLs: Array<{ redirectURI: string; state: string }>;
	exchangedCodes: Array<{ code: string; redirectURI: string }>;
	fetchedAccessTokens: Array<string>;
	listRepositoriesCalls: Array<string>;
	nextToken: {
		accessToken: string;
		tokenType: string;
		scope: string;
	};
	nextIdentity?:
		| {
				providerSubject: string;
				login: string;
				primaryEmail: string;
		  }
		| undefined;
	nextRepositories: Array<GitHubUserRepository>;
	identityError?: Error;
}

export interface FakeSessionCookies extends SessionCookies {
	values: Map<string, string>;
}

export function createDashboardTestHarness(
	overrides: Partial<DashboardConfig> = {},
): DashboardTestHarness {
	const config: DashboardConfig = {
		sessionCookieName: "dashboard_session",
		refreshCookieName: "dashboard_session_refresh",
		authStateCookieName: "dashboard_auth_state",
		publicBaseURL: "https://dashboard.example.test",
		localIngressBaseURL: undefined,
		devUsers: [{ subject: "user-1", email: "user@example.com" }],
		sessionMaxAgeSeconds: 30 * 24 * 60 * 60,
		jwtSecret: "dashboard-test-secret",
		githubInstallURL:
			"https://github.example.test/apps/platform/installations/new",
		ingressTargetHost: "platform.example.test",
		localDomainSuffix: undefined,
		github: {
			appId: "1",
			clientId: "github-client-id",
			clientSecret: "github-client-secret",
			authorizationBaseURL: "https://github.example.test",
			apiBaseURL: "https://api.github.example.test",
		},
		...overrides,
	};

	const users = new Map<string, DashboardUser>();
	const refreshSessions = new Map<string, RefreshSessionRecord>();
	const githubAccounts = new Map<string, FakeGitHubAccount>();
	const onboardingDrafts = new Map<string, DashboardOnboardingDraft>();
	let nextUserID = 1;
	let nextSessionID = 1;
	const now = new Date("2026-03-18T12:00:00Z");
	const storeEnsureInitializedCalls: Array<number> = [];

	const store: DashboardStore = {
		async ensureInitialized(): Promise<void> {
			storeEnsureInitializedCalls.push(storeEnsureInitializedCalls.length + 1);
		},
		async upsertDevUser(subject, email): Promise<DashboardUser> {
			const existing = users.get(subject);
			if (existing) {
				const updated = { ...existing, email };
				users.set(subject, updated);
				onboardingDrafts.set(updated.id, defaultOnboardingDraft());
				return updated;
			}
			const user = { id: `user-${nextUserID++}`, subject, email };
			users.set(subject, user);
			onboardingDrafts.set(user.id, defaultOnboardingDraft());
			return user;
		},
		async completeGitHubLogin(input) {
			const existing = githubAccounts.get(input.providerSubject);
			if (existing) {
				const updatedUser = { ...existing.user, email: input.primaryEmail };
				users.set(updatedUser.subject, updatedUser);
				githubAccounts.set(input.providerSubject, {
					...existing,
					user: updatedUser,
					email: input.primaryEmail,
					login: input.login,
					accessToken: input.accessToken,
					tokenType: input.tokenType,
					scope: input.scope,
					accessTokenExpiresAt: input.accessTokenExpiresAt,
					refreshToken: input.refreshToken,
					refreshTokenExpiresAt: input.refreshTokenExpiresAt,
				});
				onboardingDrafts.set(
					updatedUser.id,
					onboardingDrafts.get(updatedUser.id) ?? defaultOnboardingDraft(),
				);
				return { user: updatedUser, disposition: "login" as const };
			}

			const emailMatches = [...users.values()].filter(
				(user) => user.email.toLowerCase() === input.primaryEmail.toLowerCase(),
			);
			if (emailMatches.length > 1) {
				throw new AuthConflictError({
					code: "ambiguous_existing_user",
					message: "ambiguous_existing_user",
				});
			}
			const conflictingEmailAccount = [...githubAccounts.values()].find(
				(account) =>
					account.email.toLowerCase() === input.primaryEmail.toLowerCase() &&
					account.providerSubject !== input.providerSubject,
			);
			if (conflictingEmailAccount) {
				throw new AuthConflictError({
					code: "email_linked_to_other_github",
					message: "email_linked_to_other_github",
				});
			}
			if (emailMatches.length === 1) {
				const user = emailMatches[0];
				githubAccounts.set(input.providerSubject, {
					user,
					providerSubject: input.providerSubject,
					email: input.primaryEmail,
					login: input.login,
					accessToken: input.accessToken,
					tokenType: input.tokenType,
					scope: input.scope,
					accessTokenExpiresAt: input.accessTokenExpiresAt,
					refreshToken: input.refreshToken,
					refreshTokenExpiresAt: input.refreshTokenExpiresAt,
				});
				onboardingDrafts.set(
					user.id,
					onboardingDrafts.get(user.id) ?? defaultOnboardingDraft(),
				);
				return { user, disposition: "link" as const };
			}

			const user: DashboardUser = {
				id: `user-${nextUserID++}`,
				subject: `user_${nextUserID}`,
				email: input.primaryEmail,
			};
			users.set(user.subject, user);
			githubAccounts.set(input.providerSubject, {
				user,
				providerSubject: input.providerSubject,
				email: input.primaryEmail,
				login: input.login,
				accessToken: input.accessToken,
				tokenType: input.tokenType,
				scope: input.scope,
				accessTokenExpiresAt: input.accessTokenExpiresAt,
				refreshToken: input.refreshToken,
				refreshTokenExpiresAt: input.refreshTokenExpiresAt,
			});
			onboardingDrafts.set(user.id, defaultOnboardingDraft());
			return { user, disposition: "signup" as const };
		},
		async getGitHubAccount(userID): Promise<DashboardGitHubAccount | null> {
			const account = [...githubAccounts.values()].find(
				(entry) => entry.user.id === userID,
			);
			if (!account) {
				return null;
			}
			return {
				providerSubject: account.providerSubject,
				login: account.login,
				primaryEmail: account.email,
				accessToken: account.accessToken,
				tokenType: account.tokenType,
				scope: account.scope,
				accessTokenExpiresAt: account.accessTokenExpiresAt,
				refreshToken: account.refreshToken,
				refreshTokenExpiresAt: account.refreshTokenExpiresAt,
			};
		},
		async getOnboardingDraft(userID): Promise<DashboardOnboardingDraft> {
			const existing = onboardingDrafts.get(userID);
			if (existing) {
				return existing;
			}
			const created = defaultOnboardingDraft();
			onboardingDrafts.set(userID, created);
			return created;
		},
		async saveOnboardingDraft(
			userID,
			draft,
		): Promise<DashboardOnboardingDraft> {
			onboardingDrafts.set(userID, draft);
			return draft;
		},
		async createRefreshSession(sessionId, userID, expiresAt): Promise<void> {
			refreshSessions.set(sessionId, { id: sessionId, userID, expiresAt });
		},
		async deleteRefreshSession(sessionId): Promise<void> {
			refreshSessions.delete(sessionId);
		},
		async rotateRefreshSession(input) {
			const session = refreshSessions.get(input.sessionId);
			if (
				!session ||
				session.userID !== input.userID ||
				session.expiresAt <= input.now
			) {
				return null;
			}
			refreshSessions.delete(input.sessionId);
			refreshSessions.set(input.nextSessionId, {
				id: input.nextSessionId,
				userID: input.userID,
				expiresAt: input.expiresAt,
			});
			const user = [...users.values()].find(
				(item) => item.id === session.userID,
			);
			if (!user) {
				return null;
			}
			return user;
		},
	};

	const platform: FakePlatformGateway = {
		ensurePrincipalCalls: [],
		listProjectsCalls: [],
		createProjectCalls: [],
		listServicesCalls: [],
		inspectRepositorySourceCalls: [],
		createServiceCalls: [],
		updateServiceCalls: [],
		getServiceCalls: [],
		getServiceStatusCalls: [],
		listDomainBindingsCalls: [],
		createDomainBindingCalls: [],
		projects: [],
		services: [],
		serviceStatuses: new Map<string, DashboardServiceStatus>(),
		domainBindings: [],
		nextRepositoryInspection: undefined,
		errors: {},
		async ensurePrincipal(user): Promise<void> {
			platform.ensurePrincipalCalls.push(user);
			if (platform.errors.ensurePrincipal) {
				throw platform.errors.ensurePrincipal;
			}
		},
		async listProjects(user): Promise<Array<DashboardProject>> {
			platform.listProjectsCalls.push(user);
			if (platform.errors.listProjects) {
				throw platform.errors.listProjects;
			}
			return platform.projects;
		},
		async createProject(user, name): Promise<DashboardProject> {
			platform.createProjectCalls.push({ user, name });
			if (platform.errors.createProject) {
				throw platform.errors.createProject;
			}
			const project: DashboardProject = {
				id: `project-${platform.createProjectCalls.length}`,
				name,
				kind: "user",
			};
			platform.projects = [...platform.projects, project];
			return project;
		},
		async listServices(
			user,
			projectId,
		): Promise<Array<DashboardServiceRecord>> {
			platform.listServicesCalls.push({ user, projectId });
			if (platform.errors.listServices) {
				throw platform.errors.listServices;
			}
			return platform.services.filter(
				(service) => service.projectId === projectId,
			);
		},
		async inspectRepositorySource(
			user,
			input,
		): Promise<DashboardRepositoryInspection> {
			platform.inspectRepositorySourceCalls.push({ user, ...input });
			if (platform.errors.inspectRepositorySource) {
				throw platform.errors.inspectRepositorySource;
			}
			return (
				platform.nextRepositoryInspection ?? {
					accessState: "available",
					defaultBranch: "main",
					dockerfileCandidates: ["Dockerfile"],
					recommendedBuildRecipe: {
						dockerfilePath: "Dockerfile",
						contextDir: ".",
					},
				}
			);
		},
		async createService(user, input): Promise<DashboardServiceRecord> {
			platform.createServiceCalls.push({ user, ...input });
			if (platform.errors.createService) {
				throw platform.errors.createService;
			}
			const serviceRecord: DashboardServiceRecord = {
				id: `service-${platform.createServiceCalls.length}`,
				projectId: input.projectId,
				name: input.name,
				spec: input.source,
				sourceSummary: {
					desiredSpec: input.source,
					resolvedBinding: {
						repositorySelector: input.source.repositorySelector,
						trackedRef: input.source.trackedRef,
						accessState: "available",
						buildRecipe: input.source.buildRecipe,
					},
				},
				latestBuild: {
					buildId: `build-${platform.createServiceCalls.length}`,
					state: "queued",
					commitSha: "",
					imageDigest: "",
					failureReason: "",
				},
			};
			platform.services = [...platform.services, serviceRecord];
			platform.serviceStatuses.set(serviceRecord.id, {
				service: serviceRecord,
				allocation: {
					phase: "Pending",
					message: "",
					endpointAddr: "",
					healthy: false,
				},
			});
			return serviceRecord;
		},
		async updateService(user, input): Promise<DashboardServiceRecord> {
			platform.updateServiceCalls.push({ user, ...input });
			if (platform.errors.updateService) {
				throw platform.errors.updateService;
			}
			const current = platform.services.find(
				(service) =>
					service.projectId === input.projectId &&
					service.id === input.serviceId,
			);
			if (!current) {
				throw new Error("service not found");
			}
			const updated: DashboardServiceRecord = {
				...current,
				spec: input.source,
				sourceSummary: {
					desiredSpec: input.source,
					resolvedBinding: {
						repositorySelector: input.source.repositorySelector,
						trackedRef: input.source.trackedRef,
						accessState: "available",
						buildRecipe: input.source.buildRecipe,
					},
				},
			};
			platform.services = platform.services.map((service) =>
				service.id === updated.id ? updated : service,
			);
			const status = platform.serviceStatuses.get(updated.id);
			if (status) {
				platform.serviceStatuses.set(updated.id, {
					...status,
					service: updated,
				});
			}
			return updated;
		},
		async getService(user, input): Promise<DashboardServiceRecord> {
			platform.getServiceCalls.push({ user, ...input });
			if (platform.errors.getService) {
				throw platform.errors.getService;
			}
			const service = platform.services.find(
				(entry) =>
					entry.projectId === input.projectId && entry.id === input.serviceId,
			);
			if (!service) {
				throw new Error("service not found");
			}
			return service;
		},
		async getServiceStatus(user, input): Promise<DashboardServiceStatus> {
			platform.getServiceStatusCalls.push({ user, ...input });
			if (platform.errors.getServiceStatus) {
				throw platform.errors.getServiceStatus;
			}
			const status = platform.serviceStatuses.get(input.serviceId);
			if (!status) {
				throw new Error("service status not found");
			}
			return status;
		},
		async listDomainBindings(
			user,
			input,
		): Promise<Array<DashboardDomainBinding>> {
			platform.listDomainBindingsCalls.push({ user, ...input });
			if (platform.errors.listDomainBindings) {
				throw platform.errors.listDomainBindings;
			}
			return platform.domainBindings.filter(
				(binding) =>
					binding.projectId === input.projectId &&
					binding.serviceId === input.serviceId,
			);
		},
		async createDomainBinding(user, input): Promise<DashboardDomainBinding> {
			platform.createDomainBindingCalls.push({ user, ...input });
			if (platform.errors.createDomainBinding) {
				throw platform.errors.createDomainBinding;
			}
			const binding: DashboardDomainBinding = {
				hostname: input.hostname,
				projectId: input.projectId,
				serviceId: input.serviceId,
			};
			platform.domainBindings = [
				...platform.domainBindings.filter(
					(item) => item.hostname !== input.hostname,
				),
				binding,
			];
			return binding;
		},
	};

	const github: FakeGitHubAppUserClient = {
		authorizationURLs: [],
		exchangedCodes: [],
		fetchedAccessTokens: [],
		listRepositoriesCalls: [],
		nextToken: {
			accessToken: "github-access-token",
			tokenType: "bearer",
			scope: "read:user,user:email,repo",
		},
		nextIdentity: {
			providerSubject: "github-user-1",
			login: "octocat",
			primaryEmail: "user@example.com",
		},
		nextRepositories: [
			{
				owner: "octocat",
				name: "hello",
				fullName: "octocat/hello",
				private: false,
				defaultBranch: "main",
			},
		],
		buildAuthorizationURL(input) {
			github.authorizationURLs.push(input);
			return `https://github.example.test/login/oauth/authorize?state=${encodeURIComponent(input.state)}`;
		},
		async exchangeCode(input) {
			github.exchangedCodes.push(input);
			return github.nextToken;
		},
		async fetchIdentity(accessToken) {
			github.fetchedAccessTokens.push(accessToken);
			if (github.identityError) {
				throw github.identityError;
			}
			if (!github.nextIdentity) {
				throw new Error("missing fake GitHub identity");
			}
			return github.nextIdentity;
		},
		async listRepositories(accessToken) {
			github.listRepositoriesCalls.push(accessToken);
			return github.nextRepositories;
		},
	};

	const cookies: FakeSessionCookies = {
		values: new Map<string, string>(),
		get(name) {
			return cookies.values.get(name);
		},
		set(name, value) {
			cookies.values.set(name, value);
		},
		delete(name) {
			cookies.values.delete(name);
		},
	};

	const service = createDashboardService(config, {
		store,
		platform,
		github,
		cookies,
		now: () => now,
		randomUUID: () => `session-${nextSessionID++}`,
	});

	return {
		config,
		service,
		store,
		platform,
		github,
		cookies,
		users,
		storeEnsureInitializedCalls,
	};
}

function defaultOnboardingDraft(): DashboardOnboardingDraft {
	return {
		currentStep: "account",
		projectId: "",
		serviceId: "",
		repositorySelector: "",
		trackedRef: "",
		dockerfilePath: "",
		contextDir: "",
		containerPort: "",
		hostname: "",
	};
}
