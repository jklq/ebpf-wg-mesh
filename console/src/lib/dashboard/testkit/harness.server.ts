import { createDashboardService } from "#/lib/dashboard/core/service.server";
import {
	AuthConflictError,
	type DashboardConfig,
	type DashboardGitHubAccount,
	type DashboardOnboardingDraft,
	type DashboardService,
	type DashboardStore,
	type DashboardUser,
	type SessionCookies,
} from "#/lib/dashboard/core/types.server";
import {
	createFakeGitHubAppUserClient,
	type FakeGitHubAppUserClient,
} from "#/lib/dashboard/testkit/github.server";
import {
	createFakePlatformGateway,
	type FakePlatformGateway,
} from "#/lib/dashboard/testkit/platform.server";

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
	storeEnsureSessionUserCalls: Array<DashboardUser>;
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
	const storeEnsureSessionUserCalls: Array<DashboardUser> = [];

	const store: DashboardStore = {
		async ensureInitialized(): Promise<void> {
			storeEnsureInitializedCalls.push(storeEnsureInitializedCalls.length + 1);
		},
		async ensureSessionUser(user): Promise<void> {
			storeEnsureSessionUserCalls.push(user);
			users.set(user.subject, user);
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
			const user = [...users.values()].find((entry) => entry.id === userID);
			if (!user) {
				throw new Error(`user not found: ${userID}`);
			}
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

	const platform = createFakePlatformGateway();

	const github = createFakeGitHubAppUserClient();

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
		storeEnsureSessionUserCalls,
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
