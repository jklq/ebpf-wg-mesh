import type { PlatformGateway } from "#/lib/platform-grpc/gateway.server";

export type { PlatformGateway } from "#/lib/platform-grpc/gateway.server";

import type {
	DashboardBuilderKind,
	DashboardOnboardingDraft,
	DashboardRestartSpec,
	DashboardRollingStrategy,
	DashboardServicePosition,
	DashboardUser,
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
	restart?: Partial<DashboardRestartSpec>;
	desiredReplicaCount?: number;
	placementRegion?: string;
	rollingStrategy?: DashboardRollingStrategy;
	volumeMount?: { volumeName: string; mountPath: string } | null;
}
