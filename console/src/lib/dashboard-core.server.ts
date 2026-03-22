import {
	Data,
	Effect,
	Layer,
	ManagedRuntime,
	Option,
	Schema,
	ServiceMap,
} from "effect";

import type { DomainVerificationResult } from "#/lib/domain-dns.server";
import { normalizeHostname, verifyHostnameDNS } from "#/lib/domain-dns.server";
import {
	createSessionTokenPair,
	decodeRefreshTokenWithoutExpiryCheck,
	verifyAccessToken,
	verifyRefreshToken,
} from "#/lib/dashboard-jwt.server";
import {
	defaultOnboardingDraft,
	normalizeRepositorySelector,
	repositoryBasename,
	slugifyServiceName,
} from "#/lib/onboarding-flow";

export interface DashboardUser {
	id: string;
	subject: string;
	email: string;
}

export type DashboardProjectKind = "user" | "managed";

export interface DashboardProject {
	id: string;
	name: string;
	kind: DashboardProjectKind;
	systemKey?: string;
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

export interface DashboardBuildRecipe {
	dockerfilePath: string;
	contextDir: string;
}

export interface DashboardGitHubAccount {
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

export interface DashboardOnboardingDraft {
	currentStep: DashboardOnboardingStep;
	projectId: string;
	serviceId: string;
	repositorySelector: string;
	trackedRef: string;
	dockerfilePath: string;
	contextDir: string;
	containerPort: string;
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
}

export interface DashboardSourceSpec {
	provider: string;
	repositorySelector: string;
	trackedRef: string;
	buildRecipe?: DashboardBuildRecipe;
	containerPort?: number;
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
	failureReason: string;
}

export interface DashboardServiceSourceSummary {
	desiredSpec?: DashboardSourceSpec;
	resolvedBinding?: DashboardResolvedSourceBinding;
}

export interface DashboardServiceRecord {
	id: string;
	projectId: string;
	name: string;
	spec?: DashboardSourceSpec;
	sourceSummary?: DashboardServiceSourceSummary;
	lastSuccessfulCommitSha?: string;
	resolvedImage?: string;
	latestBuild?: DashboardBuildStatus;
}

export interface DashboardAllocationStatus {
	phase: string;
	message: string;
	endpointAddr: string;
	healthy: boolean;
}

export interface DashboardServiceStatus {
	service: DashboardServiceRecord;
	allocation?: DashboardAllocationStatus;
}

export interface DashboardDomainBinding {
	hostname: string;
	projectId: string;
	serviceId: string;
}

export interface DashboardHomeState {
	user: DashboardUser;
	githubAccount?: DashboardGitHubAccount;
	onboarding: DashboardOnboardingDraft;
	repositories: Array<GitHubUserRepository>;
	githubInstallURL?: string;
	publicBaseURL: string;
	localIngressBaseURL?: string;
	ingressTargetHost: string;
	localDomainSuffix?: string;
	project?: DashboardProject;
	service?: DashboardServiceRecord;
	serviceStatus?: DashboardServiceStatus;
	repositoryInspection?: DashboardRepositoryInspection;
	domainVerification?: DomainVerificationResult;
	domainBindings: Array<DashboardDomainBinding>;
	controlPlaneReachable: boolean;
	controlPlaneError?: string;
}

export interface DevLoginIdentity {
	subject: string;
	email: string;
}

export interface GitHubAppUserAuthConfig {
	appId: string;
	clientId: string;
	clientSecret: string;
	authorizationBaseURL: string;
	apiBaseURL: string;
}

export interface DashboardConfig {
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

interface DashboardSession {
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

export class AuthConflictError extends Data.TaggedError("AuthConflictError")<{
	readonly code: AuthConflictCode;
	readonly message: string;
}> {}

export class AuthenticationRequiredError extends Data.TaggedError(
	"AuthenticationRequiredError",
)<{
	readonly message: string;
}> {}

export class DashboardValidationError extends Data.TaggedError(
	"DashboardValidationError",
)<{
	readonly message: string;
}> {}

export class DashboardConfigError extends Data.TaggedError(
	"DashboardConfigError",
)<{
	readonly message: string;
}> {}

export class DatabaseError extends Data.TaggedError("DatabaseError")<{
	readonly operation: string;
	readonly message: string;
	readonly cause: unknown;
}> {}

export class GitHubApiError extends Data.TaggedError("GitHubApiError")<{
	readonly operation: string;
	readonly message: string;
	readonly cause: unknown;
	readonly status?: number;
}> {}

export class PlatformGatewayError extends Data.TaggedError(
	"PlatformGatewayError",
)<{
	readonly operation: string;
	readonly message: string;
	readonly cause: unknown;
	readonly grpcCode?: number;
}> {}

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
	upsertDevUser(subject: string, email: string): Promise<DashboardUser>;
	completeGitHubLogin(
		input: GitHubAccountLoginInput,
	): Promise<GitHubAccountLoginResult>;
	getGitHubAccount(userID: string): Promise<DashboardGitHubAccount | null>;
	getOnboardingDraft(userID: string): Promise<DashboardOnboardingDraft>;
	saveOnboardingDraft(
		userID: string,
		draft: DashboardOnboardingDraft,
	): Promise<DashboardOnboardingDraft>;
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
	ensurePrincipal(user: DashboardUser): Promise<void>;
	listProjects(user: DashboardUser): Promise<Array<DashboardProject>>;
	createProject(user: DashboardUser, name: string): Promise<DashboardProject>;
	listServices(
		user: DashboardUser,
		projectId: string,
	): Promise<Array<DashboardServiceRecord>>;
	inspectRepositorySource(
		user: DashboardUser,
		input: { provider: string; repositorySelector: string },
	): Promise<DashboardRepositoryInspection>;
	createService(
		user: DashboardUser,
		input: {
			projectId: string;
			name: string;
			source: DashboardSourceSpec;
		},
	): Promise<DashboardServiceRecord>;
	updateService(
		user: DashboardUser,
		input: {
			projectId: string;
			serviceId: string;
			source: DashboardSourceSpec;
		},
	): Promise<DashboardServiceRecord>;
	getService(
		user: DashboardUser,
		input: { projectId: string; serviceId: string },
	): Promise<DashboardServiceRecord>;
	getServiceStatus(
		user: DashboardUser,
		input: { projectId: string; serviceId: string },
	): Promise<DashboardServiceStatus>;
	listDomainBindings(
		user: DashboardUser,
		input: { projectId: string; serviceId: string },
	): Promise<Array<DashboardDomainBinding>>;
	createDomainBinding(
		user: DashboardUser,
		input: { projectId: string; serviceId: string; hostname: string },
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

export interface DashboardService {
	listDevLogins(): Array<DevLoginIdentity>;
	isGitHubLoginEnabled(): boolean;
	getPublicBaseURL(): string;
	beginGitHubLogin(input: { redirectTo?: string }): Promise<string>;
	completeAuthCallback(input: {
		code?: string;
		state?: string;
		subject?: string;
		email?: string;
		redirectTo?: string;
	}): Promise<string>;
	loadDashboardHome(): Promise<DashboardHomeState | null>;
	createProjectFromSession(name: string): Promise<DashboardProject>;
	inspectRepositoryFromSession(input: {
		repositorySelector: string;
	}): Promise<DashboardOnboardingDraft>;
	confirmRepositoryFromSession(input: {
		repositorySelector: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		containerPort?: string;
	}): Promise<DashboardOnboardingDraft>;
	saveHostnameFromSession(hostname: string): Promise<DashboardOnboardingDraft>;
	publishDomainFromSession(): Promise<DashboardDomainBinding>;
	clearSession(): Promise<void>;
	refreshSession(): Promise<void>;
}

interface AuthStateCookie {
	state: string;
	redirectTo: string;
}

const IdentifierSchema = Schema.String.check(
	Schema.isPattern(/^[A-Za-z_][A-Za-z0-9_]*$/),
);
const decodeIdentifier = Schema.decodeUnknownSync(IdentifierSchema);

const DevLoginIdentitySchema = Schema.Struct({
	subject: Schema.NonEmptyString,
	email: Schema.NonEmptyString,
});
const decodeDevLoginIdentity = Schema.decodeUnknownSync(DevLoginIdentitySchema);

const AuthStateCookieSchema = Schema.Struct({
	state: Schema.NonEmptyString,
	redirectTo: Schema.String,
});
const decodeAuthStateCookie = Schema.decodeUnknownSync(AuthStateCookieSchema);

export class DashboardConfigService extends ServiceMap.Service<
	DashboardConfigService,
	DashboardConfig
>()("dashboard/DashboardConfig") {}

export class DashboardStoreService extends ServiceMap.Service<
	DashboardStoreService,
	DashboardStore
>()("dashboard/DashboardStore") {}

export class PlatformGatewayService extends ServiceMap.Service<
	PlatformGatewayService,
	PlatformGateway
>()("dashboard/PlatformGateway") {}

export class GitHubAppUserClientService extends ServiceMap.Service<
	GitHubAppUserClientService,
	GitHubAppUserClient
>()("dashboard/GitHubAppUserClient") {}

export class SessionCookiesService extends ServiceMap.Service<
	SessionCookiesService,
	SessionCookies
>()("dashboard/SessionCookies") {}

export class DashboardClockService extends ServiceMap.Service<
	DashboardClockService,
	{ now: () => Date }
>()("dashboard/Clock") {}

export class DashboardUUIDService extends ServiceMap.Service<
	DashboardUUIDService,
	{ randomUUID: () => string }
>()("dashboard/UUID") {}

type DashboardStableLayer = Layer.Layer<
	| DashboardConfigService
	| DashboardStoreService
	| PlatformGatewayService
	| DashboardClockService
	| DashboardUUIDService
	| GitHubAppUserClientService,
	never,
	never
>;

type DashboardRequestServices =
	| DashboardConfigService
	| DashboardStoreService
	| PlatformGatewayService
	| DashboardClockService
	| DashboardUUIDService
	| GitHubAppUserClientService
	| SessionCookiesService;

export function createDashboardService(
	config: DashboardConfig,
	deps: DashboardDependencies,
): DashboardService {
	const requestLayer = createDashboardRequestLayer(deps.cookies);
	const runtime = ManagedRuntime.make(
		createDashboardStableLayer(config, {
			store: deps.store,
			platform: deps.platform,
			github: deps.github,
			now: deps.now,
			randomUUID: deps.randomUUID,
		}),
	);

	function withRequest<A, E>(
		program: Effect.Effect<A, E, DashboardRequestServices>,
	) {
		return program.pipe(Effect.provide(requestLayer));
	}

	function runPromise<A, E>(
		operation: string,
		program: Effect.Effect<A, E, DashboardRequestServices>,
	): Promise<A> {
		void operation;
		return runtime.runPromise(withRequest(program));
	}

	function runSync<A, E>(
		program: Effect.Effect<A, E, DashboardRequestServices>,
	): A {
		return runtime.runSync(withRequest(program));
	}

	return {
		listDevLogins() {
			return runSync(listDevLoginsEffect);
		},

		isGitHubLoginEnabled() {
			return runSync(isGitHubLoginEnabledEffect);
		},

		getPublicBaseURL() {
			return config.publicBaseURL;
		},

		beginGitHubLogin(input) {
			return runPromise("beginGitHubLogin", beginGitHubLoginEffect(input));
		},

		completeAuthCallback(input) {
			return runPromise(
				"completeAuthCallback",
				completeAuthCallbackEffect(input),
			);
		},

		loadDashboardHome() {
			return runPromise("loadDashboardHome", loadDashboardHomeEffect);
		},

		createProjectFromSession(name) {
			return runPromise(
				"createProjectFromSession",
				createProjectFromSessionEffect(name),
			);
		},

		inspectRepositoryFromSession(input) {
			return runPromise(
				"inspectRepositoryFromSession",
				inspectRepositoryFromSessionEffect(input),
			);
		},

		confirmRepositoryFromSession(input) {
			return runPromise(
				"confirmRepositoryFromSession",
				confirmRepositoryFromSessionEffect(input),
			);
		},

		saveHostnameFromSession(hostname) {
			return runPromise(
				"saveHostnameFromSession",
				saveHostnameFromSessionEffect(hostname),
			);
		},

		publishDomainFromSession() {
			return runPromise(
				"publishDomainFromSession",
				publishDomainFromSessionEffect,
			);
		},

		clearSession() {
			return runPromise("clearSession", clearSessionEffect);
		},

		refreshSession() {
			return runPromise("refreshSession", refreshSessionEffect);
		},
	};
}

export function createDashboardStableLayer(
	config: DashboardConfig,
	deps: Omit<DashboardDependencies, "cookies">,
): DashboardStableLayer {
	let layer = Layer.mergeAll(
		Layer.succeed(DashboardConfigService)(config),
		Layer.succeed(DashboardStoreService)(deps.store),
		Layer.succeed(PlatformGatewayService)(deps.platform),
		Layer.succeed(DashboardClockService)({
			now: deps.now ?? (() => new Date()),
		}),
		Layer.succeed(DashboardUUIDService)({
			randomUUID: deps.randomUUID ?? (() => crypto.randomUUID()),
		}),
	) as DashboardStableLayer;

	if (deps.github) {
		layer = Layer.merge(
			layer,
			Layer.succeed(GitHubAppUserClientService)(deps.github),
		) as DashboardStableLayer;
	}

	return layer;
}

export function createDashboardRequestLayer(
	cookies: SessionCookies,
): Layer.Layer<SessionCookiesService> {
	return Layer.succeed(SessionCookiesService)(cookies);
}

export const listDevLoginsEffect = Effect.gen(function* () {
	const config = yield* DashboardConfigService;
	return config.devUsers;
}).pipe(Effect.withSpan("dashboard.listDevLogins"));

export const isGitHubLoginEnabledEffect = Effect.gen(function* () {
	const config = yield* DashboardConfigService;
	const github = yield* Effect.serviceOption(GitHubAppUserClientService);
	return Boolean(config.github && Option.isSome(github));
}).pipe(Effect.withSpan("dashboard.isGitHubLoginEnabled"));

export const beginGitHubLoginEffect = (input: { redirectTo?: string }) =>
	Effect.gen(function* () {
		const config = yield* DashboardConfigService;
		const cookies = yield* SessionCookiesService;
		const ids = yield* DashboardUUIDService;
		const github = yield* GitHubAppUserClientService;

		if (!config.github) {
			return yield* Effect.fail(
				new AuthConflictError({
					code: "github_auth_unavailable",
					message: "GitHub sign-in is not configured",
				}),
			);
		}

		const state = ids.randomUUID();
		const redirectTo = sanitizeRedirect(input.redirectTo);
		cookies.set(
			config.authStateCookieName,
			JSON.stringify({ state, redirectTo } satisfies AuthStateCookie),
			authStateCookieOptions(config, new Date()),
		);
		return github.buildAuthorizationURL({
			redirectURI: authCallbackURL(config),
			state,
		});
	}).pipe(Effect.withSpan("dashboard.beginGitHubLogin"));

export const completeAuthCallbackEffect = (input: {
	code?: string;
	state?: string;
	subject?: string;
	email?: string;
	redirectTo?: string;
}) =>
	Effect.gen(function* () {
		const config = yield* DashboardConfigService;
		const cookies = yield* SessionCookiesService;

		if (input.code) {
			if (!config.github) {
				return yield* Effect.fail(
					new AuthConflictError({
						code: "github_auth_unavailable",
						message: "GitHub sign-in is not configured",
					}),
				);
			}

			const rawState = cookies.get(config.authStateCookieName);
			cookies.delete(config.authStateCookieName, { path: "/auth" });
			if (!rawState) {
				return yield* Effect.fail(
					new AuthConflictError({
						code: "invalid_signin_state",
						message: "missing sign-in state cookie",
					}),
				);
			}

			let cookieState: AuthStateCookie;
			try {
				cookieState = decodeAuthStateCookie(JSON.parse(rawState));
			} catch {
				return yield* Effect.fail(
					new AuthConflictError({
						code: "invalid_signin_state",
						message: "invalid sign-in state cookie",
					}),
				);
			}

			if (cookieState.state !== input.state) {
				return yield* Effect.fail(
					new AuthConflictError({
						code: "invalid_signin_state",
						message: "sign-in state mismatch",
					}),
				);
			}

			const github = yield* GitHubAppUserClientService;
			const githubCode = input.code;
			const token = yield* githubEffect("exchangeCode", () =>
				github.exchangeCode({
					code: githubCode,
					redirectURI: authCallbackURL(config),
				}),
			);
			const identity = yield* githubEffect("fetchIdentity", () =>
				github.fetchIdentity(token.accessToken),
			);
			const result = yield* storeAuthEffect("completeGitHubLogin", (store) =>
				store.completeGitHubLogin({
					providerSubject: identity.providerSubject,
					login: identity.login,
					primaryEmail: identity.primaryEmail,
					accessToken: token.accessToken,
					tokenType: token.tokenType,
					scope: token.scope,
					accessTokenExpiresAt: token.accessTokenExpiresAt,
					refreshToken: token.refreshToken,
					refreshTokenExpiresAt: token.refreshTokenExpiresAt,
				}),
			);
			return yield* signInEffect(result.user, "/");
		}

		const subject = input.subject?.trim() ?? "";
		const email = input.email?.trim() ?? "";
		if (subject === "" || email === "") {
			return yield* Effect.fail(
				new DashboardValidationError({
					message:
						"auth callback requires GitHub code or dev login subject/email",
				}),
			);
		}
		if (config.devUsers.length === 0) {
			return yield* Effect.fail(
				new AuthConflictError({
					code: "dev_auth_unavailable",
					message: "dev login is not configured",
				}),
			);
		}
		const allowed = config.devUsers.some(
			(entry) => entry.subject === subject && entry.email === email,
		);
		if (!allowed) {
			return yield* Effect.fail(
				new AuthConflictError({
					code: "invalid_dev_login",
					message: "dev login is not allowed",
				}),
			);
		}
		const user = yield* storeEffect("upsertDevUser", (store) =>
			store.upsertDevUser(subject, email),
		);
		return yield* signInEffect(user, input.redirectTo);
	}).pipe(Effect.withSpan("dashboard.completeAuthCallback"));

export const loadDashboardHomeEffect = Effect.gen(function* () {
	const session = yield* currentSessionEffect;
	if (!session) {
		return null;
	}
	const config = yield* DashboardConfigService;
	const onboarding = yield* storeEffect("getOnboardingDraft", (store) =>
		store.getOnboardingDraft(session.user.id),
	);
	const githubAccount = yield* storeEffect("getGitHubAccount", (store) =>
		store.getGitHubAccount(session.user.id),
	);
	const repositories = githubAccount?.accessToken
		? yield* listGitHubRepositoriesEffect(githubAccount.accessToken).pipe(
				Effect.catchTag("GitHubApiError", () => Effect.succeed([])),
			)
		: [];

	const baseState = {
		user: session.user,
		githubAccount: githubAccount ?? undefined,
		onboarding,
		repositories,
		githubInstallURL: config.githubInstallURL,
		publicBaseURL: config.publicBaseURL,
		localIngressBaseURL: config.localIngressBaseURL,
		ingressTargetHost: config.ingressTargetHost,
		localDomainSuffix: config.localDomainSuffix,
		domainBindings: [],
		controlPlaneReachable: true,
	} satisfies DashboardHomeState;

	return yield* Effect.gen(function* () {
		yield* platformEffect("ensurePrincipal", (platform) =>
			platform.ensurePrincipal(session.user),
		);
		const projects = yield* platformEffect("listProjects", (platform) =>
			platform.listProjects(session.user),
		);
		let project = onboarding.projectId
			? projects.find((entry) => entry.id === onboarding.projectId)
			: undefined;
		if (!project && onboarding.repositorySelector) {
			project = projects.find(
				(entry) => entry.name === onboarding.repositorySelector,
			);
		}
		const repositoryInspection = onboarding.repositorySelector
			? yield* platformEffect("inspectRepositorySource", (platform) =>
					platform.inspectRepositorySource(session.user, {
						provider: "github",
						repositorySelector: onboarding.repositorySelector,
					}),
				)
			: undefined;

		let reconciledDraft = onboarding;
		let service: DashboardServiceRecord | undefined;
		let serviceStatus: DashboardServiceStatus | undefined;
		let domainBindings: Array<DashboardDomainBinding> = [];
		if (project && onboarding.serviceId) {
			service = yield* platformEffect("getService", (platform) =>
				platform.getService(session.user, {
					projectId: project.id,
					serviceId: onboarding.serviceId,
				}),
			).pipe(
				Effect.catchTag("PlatformGatewayError", () =>
					Effect.succeed(undefined),
				),
			);
		}
		if (!service && project && onboarding.repositorySelector) {
			const services = yield* platformEffect("listServices", (platform) =>
				platform.listServices(session.user, project.id),
			).pipe(Effect.catchTag("PlatformGatewayError", () => Effect.succeed([])));
			service = services.find(
				(entry) =>
					entry.spec?.repositorySelector === onboarding.repositorySelector,
			);
		}
		if (project && service) {
			serviceStatus = yield* platformEffect("getServiceStatus", (platform) =>
				platform.getServiceStatus(session.user, {
					projectId: project.id,
					serviceId: service.id,
				}),
			).pipe(
				Effect.catchTag("PlatformGatewayError", () =>
					Effect.succeed(undefined),
				),
			);
			domainBindings = yield* platformEffect("listDomainBindings", (platform) =>
				platform.listDomainBindings(session.user, {
					projectId: project.id,
					serviceId: service.id,
				}),
			).pipe(Effect.catchTag("PlatformGatewayError", () => Effect.succeed([])));
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				repositoryInspection,
				project,
				service,
				serviceStatus,
				domainBindings,
			);
		} else if (onboarding.projectId || onboarding.serviceId) {
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				repositoryInspection,
				project,
				undefined,
				undefined,
				[],
			);
		}
		if (!onboardingDraftEquals(onboarding, reconciledDraft)) {
			reconciledDraft = yield* saveOnboardingDraftEffect(
				session.user.id,
				reconciledDraft,
			);
		}

		const domainVerification = reconciledDraft.hostname
			? yield* Effect.tryPromise({
					try: () =>
						verifyHostnameDNS(
							reconciledDraft.hostname,
							config.ingressTargetHost,
							undefined,
							config.localDomainSuffix,
						),
					catch: (cause) =>
						new DashboardValidationError({
							message: formatError(cause),
						}),
				}).pipe(
					Effect.catchTag("DashboardValidationError", () =>
						Effect.succeed(undefined),
					),
				)
			: undefined;

		return {
			...baseState,
			onboarding: reconciledDraft,
			project,
			service,
			serviceStatus,
			repositoryInspection,
			domainVerification,
			domainBindings,
		} satisfies DashboardHomeState;
	}).pipe(
		Effect.catchTag("PlatformGatewayError", (error) =>
			Effect.succeed({
				...baseState,
				controlPlaneReachable: false,
				controlPlaneError: error.message,
			} satisfies DashboardHomeState),
		),
	);
}).pipe(Effect.withSpan("dashboard.loadDashboardHome"));

export const createProjectFromSessionEffect = (name: string) =>
	Effect.gen(function* () {
		const session = yield* requireSessionEffect;
		const projectName = name.trim();
		if (projectName === "") {
			return yield* Effect.fail(
				new DashboardValidationError({
					message: "project name is required",
				}),
			);
		}
		yield* platformEffect("ensurePrincipal", (platform) =>
			platform.ensurePrincipal(session.user),
		);
		return yield* platformEffect("createProject", (platform) =>
			platform.createProject(session.user, projectName),
		);
	}).pipe(Effect.withSpan("dashboard.createProjectFromSession"));

export const inspectRepositoryFromSessionEffect = (input: {
	repositorySelector: string;
}) =>
	Effect.gen(function* () {
		const session = yield* requireSessionEffect;
		const selector = normalizeRepositorySelector(input.repositorySelector);
		yield* platformEffect("ensurePrincipal", (platform) =>
			platform.ensurePrincipal(session.user),
		);
		const inspection = yield* platformEffect(
			"inspectRepositorySource",
			(platform) =>
				platform.inspectRepositorySource(session.user, {
					provider: "github",
					repositorySelector: selector,
				}),
		);
		const draft = yield* loadOnboardingDraftEffect(session.user.id);
		const recommended = inspection.recommendedBuildRecipe;
		const selectorChanged = draft.repositorySelector !== selector;
		const nextDraft: DashboardOnboardingDraft = {
			...draft,
			currentStep: "repository",
			projectId: selectorChanged ? "" : draft.projectId,
			serviceId: selectorChanged ? "" : draft.serviceId,
			repositorySelector: selector,
			trackedRef:
				selectorChanged || draft.trackedRef === ""
					? inspection.defaultBranch
					: draft.trackedRef,
			dockerfilePath:
				selectorChanged || draft.dockerfilePath === ""
					? (recommended?.dockerfilePath ?? "")
					: draft.dockerfilePath,
			contextDir:
				selectorChanged || draft.contextDir === ""
					? (recommended?.contextDir ?? "")
					: draft.contextDir,
			containerPort: selectorChanged ? "" : draft.containerPort,
			hostname: selectorChanged ? "" : draft.hostname,
		};
		return yield* saveOnboardingDraftEffect(session.user.id, nextDraft);
	}).pipe(Effect.withSpan("dashboard.inspectRepositoryFromSession"));

export const confirmRepositoryFromSessionEffect = (input: {
	repositorySelector: string;
	trackedRef?: string;
	dockerfilePath?: string;
	contextDir?: string;
	containerPort?: string;
}) =>
	Effect.gen(function* () {
		const session = yield* requireSessionEffect;
		const selector = normalizeRepositorySelector(input.repositorySelector);
		yield* platformEffect("ensurePrincipal", (platform) =>
			platform.ensurePrincipal(session.user),
		);
		const inspection = yield* platformEffect(
			"inspectRepositorySource",
			(platform) =>
				platform.inspectRepositorySource(session.user, {
					provider: "github",
					repositorySelector: selector,
				}),
		);
		if (inspection.accessState !== "available") {
			return yield* Effect.fail(
				new DashboardValidationError({
					message: "Repository access is not available yet.",
				}),
			);
		}
		const dockerfilePath =
			input.dockerfilePath?.trim() ||
			inspection.recommendedBuildRecipe?.dockerfilePath ||
			"";
		if (dockerfilePath === "") {
			return yield* Effect.fail(
				new DashboardValidationError({
					message:
						"No Dockerfile was detected for this repository. Pick a repo with a Dockerfile or add one first.",
				}),
			);
		}
		const contextDir =
			input.contextDir?.trim() ||
			inspection.recommendedBuildRecipe?.contextDir ||
			".";
		const containerPort = parseContainerPort(input.containerPort);
		const trackedRef =
			input.trackedRef?.trim() || inspection.defaultBranch || "main";
		const projects = yield* platformEffect("listProjects", (platform) =>
			platform.listProjects(session.user),
		);
		const projectName = selector;
		const project =
			projects.find((entry) => entry.name === projectName) ??
			(yield* platformEffect("createProject", (platform) =>
				platform.createProject(session.user, projectName),
			));
		const services = yield* platformEffect("listServices", (platform) =>
			platform.listServices(session.user, project.id),
		);
		const existingService = services.find(
			(entry) => entry.spec?.repositorySelector === selector,
		);
		const desiredSource: DashboardSourceSpec = {
			provider: "github",
			repositorySelector: selector,
			trackedRef,
			buildRecipe: {
				dockerfilePath,
				contextDir,
			},
			containerPort,
		};
		const service = existingService
			? yield* platformEffect("updateService", (platform) =>
					platform.updateService(session.user, {
						projectId: project.id,
						serviceId: existingService.id,
						source: desiredSource,
					}),
				)
			: yield* platformEffect("createService", (platform) =>
					platform.createService(session.user, {
						projectId: project.id,
						name: nextServiceName(services, selector),
						source: desiredSource,
					}),
				);
		return yield* saveOnboardingDraftEffect(session.user.id, {
			currentStep: "build",
			projectId: project.id,
			serviceId: service.id,
			repositorySelector: selector,
			trackedRef,
			dockerfilePath,
			contextDir,
			containerPort: String(containerPort),
			hostname: "",
		});
	}).pipe(Effect.withSpan("dashboard.confirmRepositoryFromSession"));

export const saveHostnameFromSessionEffect = (hostname: string) =>
	Effect.gen(function* () {
		const session = yield* requireSessionEffect;
		const normalizedHostname = normalizeHostname(hostname);
		const draft = yield* loadOnboardingDraftEffect(session.user.id);
		if (!draft.projectId || !draft.serviceId) {
			return yield* Effect.fail(
				new DashboardValidationError({
					message: "Create a service before connecting a domain.",
				}),
			);
		}
		return yield* saveOnboardingDraftEffect(session.user.id, {
			...draft,
			currentStep: "domain",
			hostname: normalizedHostname,
		});
	}).pipe(Effect.withSpan("dashboard.saveHostnameFromSession"));

export const publishDomainFromSessionEffect = Effect.gen(function* () {
	const session = yield* requireSessionEffect;
	const config = yield* DashboardConfigService;
	const draft = yield* loadOnboardingDraftEffect(session.user.id);
	if (!draft.projectId || !draft.serviceId) {
		return yield* Effect.fail(
			new DashboardValidationError({
				message: "Create a service before publishing a domain.",
			}),
		);
	}
	if (!draft.hostname) {
		return yield* Effect.fail(
			new DashboardValidationError({
				message: "Enter a hostname first.",
			}),
		);
	}
	const serviceStatus = yield* platformEffect("getServiceStatus", (platform) =>
		platform.getServiceStatus(session.user, {
			projectId: draft.projectId,
			serviceId: draft.serviceId,
		}),
	);
	if (!buildHealthyAndReady(serviceStatus)) {
		return yield* Effect.fail(
			new DashboardValidationError({
				message:
					"Wait for the latest build to succeed and the deployment to become healthy before publishing a domain.",
			}),
		);
	}
	const verification = yield* Effect.tryPromise({
		try: () =>
			verifyHostnameDNS(
				draft.hostname,
				config.ingressTargetHost,
				undefined,
				config.localDomainSuffix,
			),
		catch: (cause) =>
			new DashboardValidationError({
				message: formatError(cause),
			}),
	});
	if (verification.state !== "verified") {
		return yield* Effect.fail(
			new DashboardValidationError({
				message: "DNS has not verified yet for this hostname.",
			}),
		);
	}
	const binding = yield* platformEffect("createDomainBinding", (platform) =>
		platform.createDomainBinding(session.user, {
			projectId: draft.projectId,
			serviceId: draft.serviceId,
			hostname: draft.hostname,
		}),
	);
	yield* saveOnboardingDraftEffect(session.user.id, {
		...draft,
		currentStep: "domain",
	});
	return binding;
}).pipe(Effect.withSpan("dashboard.publishDomainFromSession"));

export const clearSessionEffect = Effect.gen(function* () {
	const config = yield* DashboardConfigService;
	const cookies = yield* SessionCookiesService;
	const refreshToken = cookies.get(config.refreshCookieName);
	if (refreshToken) {
		const refresh = readRefreshToken(config, refreshToken);
		if (refresh) {
			yield* storeEffect("ensureInitialized", (store) =>
				store.ensureInitialized(),
			);
			yield* storeEffect("deleteRefreshSession", (store) =>
				store.deleteRefreshSession(refresh.sessionId),
			);
		}
	}
	clearAuthCookies(cookies, config);
}).pipe(Effect.withSpan("dashboard.clearSession"));

function authCallbackURL(config: DashboardConfig): string {
	return `${config.publicBaseURL.replace(/\/$/, "")}/auth/callback`;
}

const signInEffect = (user: DashboardUser, redirectTo?: string) =>
	Effect.gen(function* () {
		const config = yield* DashboardConfigService;
		const clock = yield* DashboardClockService;
		const ids = yield* DashboardUUIDService;
		const cookies = yield* SessionCookiesService;

		yield* storeEffect("ensureInitialized", (store) =>
			store.ensureInitialized(),
		);
		const now = clock.now();
		const refreshSessionId = ids.randomUUID();
		const tokens = createSessionTokenPair(config, user, refreshSessionId, now);
		yield* storeEffect("createRefreshSession", (store) =>
			store.createRefreshSession(
				tokens.refreshSessionId,
				user.id,
				tokens.refreshTokenExpiresAt,
			),
		);
		cookies.set(
			config.sessionCookieName,
			tokens.accessToken,
			sessionCookieOptions(config, tokens.accessTokenExpiresAt),
		);
		cookies.set(
			config.refreshCookieName,
			tokens.refreshToken,
			refreshCookieOptions(config, tokens.refreshTokenExpiresAt),
		);
		yield* platformEffect("ensurePrincipal", (platform) =>
			platform.ensurePrincipal(user),
		).pipe(
			Effect.catchTag("PlatformGatewayError", () => Effect.succeed(undefined)),
		);
		return sanitizeRedirect(redirectTo);
	}).pipe(Effect.withSpan("dashboard.signIn"));

const currentSessionEffect = Effect.gen(function* () {
	const config = yield* DashboardConfigService;
	const cookies = yield* SessionCookiesService;
	const clock = yield* DashboardClockService;
	const now = clock.now();

	const accessToken = cookies.get(config.sessionCookieName);
	if (accessToken) {
		const access = readAccessToken(config, accessToken, now);
		if (access) {
			return { user: access.user };
		}
	}

	return yield* refreshSessionFromCookiesEffect;
}).pipe(Effect.withSpan("dashboard.currentSession"));

export const refreshSessionEffect = Effect.gen(function* () {
	const session = yield* refreshSessionFromCookiesEffect;
	if (!session) {
		return yield* Effect.fail(
			new AuthenticationRequiredError({
				message: "authentication required",
			}),
		);
	}
}).pipe(Effect.withSpan("dashboard.refreshSession"));

const requireSessionEffect = Effect.gen(function* () {
	const session = yield* currentSessionEffect;
	if (!session) {
		return yield* Effect.fail(
			new AuthenticationRequiredError({
				message: "authentication required",
			}),
		);
	}
	return session;
}).pipe(Effect.withSpan("dashboard.requireSession"));

const loadOnboardingDraftEffect = (userID: string) =>
	storeEffect("getOnboardingDraft", (store) =>
		store.getOnboardingDraft(userID),
	);

const saveOnboardingDraftEffect = (
	userID: string,
	draft: DashboardOnboardingDraft,
) =>
	storeEffect("saveOnboardingDraft", (store) =>
		store.saveOnboardingDraft(userID, draft),
	);

const listGitHubRepositoriesEffect = (accessToken: string) =>
	GitHubAppUserClientService.use((github) =>
		Effect.tryPromise({
			try: () => github.listRepositories(accessToken),
			catch: (cause) => toGitHubApiError("listRepositories", cause),
		}),
	).pipe(Effect.withSpan("dashboard.github.listRepositories"));

function reconcileOnboardingDraft(
	draft: DashboardOnboardingDraft,
	inspection: DashboardRepositoryInspection | undefined,
	project: DashboardProject | undefined,
	service: DashboardServiceRecord | undefined,
	serviceStatus: DashboardServiceStatus | undefined,
	domainBindings: Array<DashboardDomainBinding>,
): DashboardOnboardingDraft {
	const source = service?.spec;
	const hostname = draft.hostname || domainBindings[0]?.hostname || "";
	const currentStep = deriveCurrentStep({
		draft,
		project,
		service,
		serviceStatus,
		hostname,
		domainBindings,
	});
	return {
		...defaultOnboardingDraft(),
		...draft,
		currentStep,
		projectId: project?.id ?? "",
		serviceId: service?.id ?? "",
		repositorySelector:
			draft.repositorySelector || source?.repositorySelector || "",
		trackedRef:
			draft.trackedRef || source?.trackedRef || inspection?.defaultBranch || "",
		dockerfilePath:
			draft.dockerfilePath ||
			source?.buildRecipe?.dockerfilePath ||
			inspection?.recommendedBuildRecipe?.dockerfilePath ||
			"",
		contextDir:
			draft.contextDir ||
			source?.buildRecipe?.contextDir ||
			inspection?.recommendedBuildRecipe?.contextDir ||
			"",
		containerPort:
			draft.containerPort ||
			(source?.containerPort ? String(source.containerPort) : ""),
		hostname,
	};
}

function deriveCurrentStep(input: {
	draft: DashboardOnboardingDraft;
	project: DashboardProject | undefined;
	service: DashboardServiceRecord | undefined;
	serviceStatus: DashboardServiceStatus | undefined;
	hostname: string;
	domainBindings: Array<DashboardDomainBinding>;
}): DashboardOnboardingStep {
	if (
		input.hostname ||
		input.domainBindings.length > 0 ||
		buildHealthyAndReady(input.serviceStatus ?? undefined)
	) {
		return "domain";
	}
	if (input.project && input.service) {
		return "build";
	}
	if (input.draft.repositorySelector) {
		return "repository";
	}
	return "account";
}

function onboardingDraftEquals(
	left: DashboardOnboardingDraft,
	right: DashboardOnboardingDraft,
): boolean {
	return (
		left.currentStep === right.currentStep &&
		left.projectId === right.projectId &&
		left.serviceId === right.serviceId &&
		left.repositorySelector === right.repositorySelector &&
		left.trackedRef === right.trackedRef &&
		left.dockerfilePath === right.dockerfilePath &&
		left.contextDir === right.contextDir &&
		left.containerPort === right.containerPort &&
		left.hostname === right.hostname
	);
}

function parseContainerPort(raw: string | undefined): number {
	const value = raw?.trim() ?? "";
	if (value === "") {
		return 8080;
	}
	const port = Number.parseInt(value, 10);
	if (!Number.isInteger(port) || port < 1 || port > 65535) {
		throw new DashboardValidationError({
			message: "Container port must be an integer between 1 and 65535.",
		});
	}
	return port;
}

function nextServiceName(
	services: Array<DashboardServiceRecord>,
	repositorySelector: string,
): string {
	const base = slugifyServiceName(repositoryBasename(repositorySelector));
	const names = new Set(services.map((service) => service.name));
	if (!names.has(base)) {
		return base;
	}
	for (let index = 2; index < 1000; index += 1) {
		const candidate = `${base}-${index}`;
		if (!names.has(candidate)) {
			return candidate;
		}
	}
	return `${base}-${Date.now()}`;
}

function buildHealthyAndReady(
	status: DashboardServiceStatus | undefined,
): boolean {
	return (
		status?.service.latestBuild?.state === "succeeded" &&
		status.allocation?.healthy === true
	);
}

function storeEffect<A>(
	operation: string,
	run: (store: DashboardStore) => Promise<A>,
): Effect.Effect<A, DatabaseError, DashboardStoreService> {
	return DashboardStoreService.use((store) =>
		Effect.tryPromise({
			try: () => run(store),
			catch: (cause) => toStrictDatabaseError(operation, cause),
		}).pipe(Effect.withSpan(`dashboard.store.${operation}`)),
	);
}

function storeAuthEffect<A>(
	operation: string,
	run: (store: DashboardStore) => Promise<A>,
) {
	return DashboardStoreService.use((store) =>
		Effect.tryPromise({
			try: () => run(store),
			catch: (cause) => toDatabaseError(operation, cause),
		}).pipe(Effect.withSpan(`dashboard.store.${operation}`)),
	);
}

function githubEffect<A>(
	operation: string,
	run: () => Promise<A>,
): Effect.Effect<A, AuthConflictError | GitHubApiError> {
	return Effect.tryPromise({
		try: () => run(),
		catch: (cause) => toGitHubApiError(operation, cause),
	}).pipe(Effect.withSpan(`dashboard.github.${operation}`));
}

function platformEffect<A>(
	operation: string,
	run: (platform: PlatformGateway) => Promise<A>,
): Effect.Effect<A, PlatformGatewayError, PlatformGatewayService> {
	return PlatformGatewayService.use((platform) =>
		Effect.tryPromise({
			try: () => run(platform),
			catch: (cause) => toPlatformGatewayError(operation, cause),
		}).pipe(Effect.withSpan(`dashboard.platform.${operation}`)),
	);
}

function toDatabaseError(
	operation: string,
	cause: unknown,
): DatabaseError | AuthConflictError {
	if (cause instanceof AuthConflictError || cause instanceof DatabaseError) {
		return cause;
	}
	return new DatabaseError({
		operation,
		message: formatError(cause),
		cause,
	});
}

function toStrictDatabaseError(
	operation: string,
	cause: unknown,
): DatabaseError {
	if (cause instanceof DatabaseError) {
		return cause;
	}
	return new DatabaseError({
		operation,
		message: formatError(cause),
		cause,
	});
}

function toGitHubApiError(
	operation: string,
	cause: unknown,
): AuthConflictError | GitHubApiError {
	if (cause instanceof AuthConflictError || cause instanceof GitHubApiError) {
		return cause;
	}
	return new GitHubApiError({
		operation,
		message: formatError(cause),
		cause,
		status: readStatus(cause),
	});
}

function toPlatformGatewayError(
	operation: string,
	cause: unknown,
): PlatformGatewayError {
	if (cause instanceof PlatformGatewayError) {
		return cause;
	}
	return new PlatformGatewayError({
		operation,
		message: formatError(cause),
		cause,
		grpcCode: readGrpcCode(cause),
	});
}

function readStatus(value: unknown): number | undefined {
	if (
		value &&
		typeof value === "object" &&
		"status" in value &&
		typeof value.status === "number"
	) {
		return value.status;
	}
	return undefined;
}

function readGrpcCode(value: unknown): number | undefined {
	if (
		value &&
		typeof value === "object" &&
		"code" in value &&
		typeof value.code === "number"
	) {
		return value.code;
	}
	return undefined;
}

export function sessionCookieOptions(
	config: DashboardConfig,
	expiresAt: Date,
): SessionCookieOptions {
	return {
		httpOnly: true,
		path: "/",
		sameSite: "lax",
		secure: shouldUseSecureCookies(config),
		expires: expiresAt,
	};
}

export function refreshCookieOptions(
	config: DashboardConfig,
	expiresAt: Date,
): SessionCookieOptions {
	return {
		httpOnly: true,
		path: "/",
		sameSite: "lax",
		secure: shouldUseSecureCookies(config),
		expires: expiresAt,
	};
}

export function authStateCookieOptions(
	config: DashboardConfig,
	currentTime: Date,
): SessionCookieOptions {
	return {
		httpOnly: true,
		path: "/auth",
		sameSite: "lax",
		secure: shouldUseSecureCookies(config),
		expires: new Date(currentTime.getTime() + 10 * 60 * 1000),
	};
}

function shouldUseSecureCookies(config: DashboardConfig): boolean {
	return (
		config.publicBaseURL.startsWith("https://") &&
		!config.localDomainSuffix?.trim()
	);
}

const refreshSessionFromCookiesEffect = Effect.gen(function* () {
	const config = yield* DashboardConfigService;
	const cookies = yield* SessionCookiesService;
	const clock = yield* DashboardClockService;
	const ids = yield* DashboardUUIDService;
	const now = clock.now();
	const refreshToken = cookies.get(config.refreshCookieName);
	if (!refreshToken) {
		clearAuthCookies(cookies, config);
		return null;
	}

	const refresh = readRefreshToken(config, refreshToken, now);
	if (!refresh) {
		clearAuthCookies(cookies, config);
		return null;
	}

	yield* storeEffect("ensureInitialized", (store) => store.ensureInitialized());
	const nextSessionId = ids.randomUUID();
	const nextRefreshExpiresAt = new Date(
		now.getTime() + config.sessionMaxAgeSeconds * 1000,
	);
	const user = yield* storeEffect("rotateRefreshSession", (store) =>
		store.rotateRefreshSession({
			sessionId: refresh.sessionId,
			userID: refresh.user.id,
			now,
			nextSessionId,
			expiresAt: nextRefreshExpiresAt,
		}),
	);
	if (!user) {
		clearAuthCookies(cookies, config);
		return null;
	}

	const tokens = createSessionTokenPair(config, user, nextSessionId, now);
	cookies.set(
		config.sessionCookieName,
		tokens.accessToken,
		sessionCookieOptions(config, tokens.accessTokenExpiresAt),
	);
	cookies.set(
		config.refreshCookieName,
		tokens.refreshToken,
		refreshCookieOptions(config, tokens.refreshTokenExpiresAt),
	);
	return { user };
}).pipe(Effect.withSpan("dashboard.refreshSessionFromCookies"));

function clearAuthCookies(cookies: SessionCookies, config: DashboardConfig) {
	cookies.delete(config.sessionCookieName, { path: "/" });
	cookies.delete(config.refreshCookieName, { path: "/" });
}

function readAccessToken(config: DashboardConfig, token: string, now: Date) {
	try {
		return verifyAccessToken(token, config, now);
	} catch {
		return null;
	}
}

function readRefreshToken(config: DashboardConfig, token: string, now?: Date) {
	try {
		return now
			? verifyRefreshToken(token, config, now)
			: decodeRefreshTokenWithoutExpiryCheck(token, config);
	} catch {
		return null;
	}
}

export function sanitizeRedirect(value?: string): string {
	if (!value || !value.startsWith("/")) {
		return "/";
	}
	return value;
}

export function formatError(error: unknown): string {
	if (error && typeof error === "object" && "message" in error) {
		return String(error.message);
	}
	return "unknown error";
}

export function parseIdentifier(raw: string): string {
	try {
		return decodeIdentifier(raw);
	} catch {
		throw new DashboardConfigError({
			message: `invalid dashboard schema identifier: ${raw}`,
		});
	}
}

export function parseDevUsers(raw: string): Array<DevLoginIdentity> {
	if (raw.trim() === "") {
		return [];
	}
	return raw
		.split(";")
		.map((entry) => entry.trim())
		.filter((entry) => entry !== "")
		.flatMap((entry) => {
			const [subject, email] = entry.split(":", 2);
			try {
				return [
					decodeDevLoginIdentity({
						subject: (subject ?? "").trim(),
						email: (email ?? "").trim(),
					}),
				];
			} catch {
				return [];
			}
		});
}
