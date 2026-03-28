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

	constructor(input: {
		operation: string;
		message: string;
		cause: unknown;
	}) {
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

interface DashboardRuntime {
	config: DashboardConfig;
	store: DashboardStore;
	platform: PlatformGateway;
	cookies: SessionCookies;
	github?: GitHubAppUserClient;
	now: () => Date;
	randomUUID: () => string;
}

export function createDashboardService(
	config: DashboardConfig,
	deps: DashboardDependencies,
): DashboardService {
	const runtime: DashboardRuntime = {
		config,
		store: deps.store,
		platform: deps.platform,
		cookies: deps.cookies,
		github: deps.github,
		now: deps.now ?? (() => new Date()),
		randomUUID: deps.randomUUID ?? (() => crypto.randomUUID()),
	};

	return {
		listDevLogins() {
			return config.devUsers;
		},

		isGitHubLoginEnabled() {
			return Boolean(config.github && runtime.github);
		},

		getPublicBaseURL() {
			return config.publicBaseURL;
		},

		beginGitHubLogin(input) {
			return beginGitHubLogin(runtime, input);
		},

		completeAuthCallback(input) {
			return completeAuthCallback(runtime, input);
		},

		loadDashboardHome() {
			return loadDashboardHome(runtime);
		},

		createProjectFromSession(name) {
			return createProjectFromSession(runtime, name);
		},

		inspectRepositoryFromSession(input) {
			return inspectRepositoryFromSession(runtime, input);
		},

		confirmRepositoryFromSession(input) {
			return confirmRepositoryFromSession(runtime, input);
		},

		saveHostnameFromSession(hostname) {
			return saveHostnameFromSession(runtime, hostname);
		},

		publishDomainFromSession() {
			return publishDomainFromSession(runtime);
		},

		clearSession() {
			return clearSession(runtime);
		},

		refreshSession() {
			return refreshSession(runtime);
		},
	};
}

async function beginGitHubLogin(
	runtime: DashboardRuntime,
	input: { redirectTo?: string },
): Promise<string> {
	const { config, cookies, github } = runtime;
	if (!config.github || !github) {
		throw new AuthConflictError({
			code: "github_auth_unavailable",
			message: "GitHub sign-in is not configured",
		});
	}

	const state = runtime.randomUUID();
	const redirectTo = sanitizeRedirect(input.redirectTo);
	cookies.set(
		config.authStateCookieName,
		JSON.stringify({ state, redirectTo } satisfies AuthStateCookie),
		authStateCookieOptions(config, runtime.now()),
	);
	return github.buildAuthorizationURL({
		redirectURI: authCallbackURL(config),
		state,
	});
}

async function completeAuthCallback(
	runtime: DashboardRuntime,
	input: {
		code?: string;
		state?: string;
		subject?: string;
		email?: string;
		redirectTo?: string;
	},
): Promise<string> {
	const { config, cookies, github } = runtime;
	await storeCall(runtime, "ensureInitialized", (store) =>
		store.ensureInitialized(),
	);

	if (input.code) {
		if (!config.github || !github) {
			throw new AuthConflictError({
				code: "github_auth_unavailable",
				message: "GitHub sign-in is not configured",
			});
		}

		const rawState = cookies.get(config.authStateCookieName);
		cookies.delete(config.authStateCookieName, { path: "/auth" });
		if (!rawState) {
			throw new AuthConflictError({
				code: "invalid_signin_state",
				message: "missing sign-in state cookie",
			});
		}

		const cookieState = parseAuthStateCookie(rawState);
		if (cookieState.state !== input.state) {
			throw new AuthConflictError({
				code: "invalid_signin_state",
				message: "sign-in state mismatch",
			});
		}

		const token = await githubCall(runtime, "exchangeCode", () =>
			github.exchangeCode({
				code: input.code ?? "",
				redirectURI: authCallbackURL(config),
			}),
		);
		const identity = await githubCall(runtime, "fetchIdentity", () =>
			github.fetchIdentity(token.accessToken),
		);
		const result = await storeAuthCall(runtime, "completeGitHubLogin", (store) =>
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
		return signIn(runtime, result.user, "/");
	}

	const subject = input.subject?.trim() ?? "";
	const email = input.email?.trim() ?? "";
	if (subject === "" || email === "") {
		throw new DashboardValidationError({
			message:
				"auth callback requires GitHub code or dev login subject/email",
		});
	}
	if (config.devUsers.length === 0) {
		throw new AuthConflictError({
			code: "dev_auth_unavailable",
			message: "dev login is not configured",
		});
	}
	const allowed = config.devUsers.some(
		(entry) => entry.subject === subject && entry.email === email,
	);
	if (!allowed) {
		throw new AuthConflictError({
			code: "invalid_dev_login",
			message: "dev login is not allowed",
		});
	}

	const user = await storeCall(runtime, "upsertDevUser", (store) =>
		store.upsertDevUser(subject, email),
	);
	return signIn(runtime, user, input.redirectTo);
}

async function loadDashboardHome(
	runtime: DashboardRuntime,
): Promise<DashboardHomeState | null> {
	const session = await currentSession(runtime);
	if (!session) {
		return null;
	}

	const { config } = runtime;
	const onboarding = await storeCall(runtime, "getOnboardingDraft", (store) =>
		store.getOnboardingDraft(session.user.id),
	);
	const githubAccount = await storeCall(runtime, "getGitHubAccount", (store) =>
		store.getGitHubAccount(session.user.id),
	);
	const repositories = githubAccount?.accessToken
		? await listGitHubRepositories(runtime, githubAccount.accessToken)
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

	try {
		await platformCall(runtime, "ensurePrincipal", (platform) =>
			platform.ensurePrincipal(session.user),
		);
		const projects = await platformCall(runtime, "listProjects", (platform) =>
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
			? await platformCall(runtime, "inspectRepositorySource", (platform) =>
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
			service = await safePlatformCall(runtime, "getService", (platform) =>
				platform.getService(session.user, {
					projectId: project.id,
					serviceId: onboarding.serviceId,
				}),
			);
		}

		if (!service && project && onboarding.repositorySelector) {
			const services =
				(await safePlatformCall(runtime, "listServices", (platform) =>
					platform.listServices(session.user, project.id),
				)) ?? [];
			service = services.find(
				(entry) =>
					entry.spec?.repositorySelector === onboarding.repositorySelector,
			);
		}

		if (project && service) {
			serviceStatus = await safePlatformCall(
				runtime,
				"getServiceStatus",
				(platform) =>
					platform.getServiceStatus(session.user, {
						projectId: project.id,
						serviceId: service.id,
					}),
			);
			domainBindings =
				(await safePlatformCall(runtime, "listDomainBindings", (platform) =>
					platform.listDomainBindings(session.user, {
						projectId: project.id,
						serviceId: service.id,
					}),
				)) ?? [];
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
			reconciledDraft = await saveOnboardingDraft(
				runtime,
				session.user.id,
				reconciledDraft,
			);
		}

		const domainVerification = reconciledDraft.hostname
			? await safeVerifyHostname(runtime, reconciledDraft.hostname)
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
	} catch (error) {
		if (error instanceof PlatformGatewayError) {
			return {
				...baseState,
				controlPlaneReachable: false,
				controlPlaneError: error.message,
			} satisfies DashboardHomeState;
		}
		throw error;
	}
}

async function createProjectFromSession(
	runtime: DashboardRuntime,
	name: string,
): Promise<DashboardProject> {
	const session = await requireSession(runtime);
	const projectName = name.trim();
	if (projectName === "") {
		throw new DashboardValidationError({
			message: "project name is required",
		});
	}
	await platformCall(runtime, "ensurePrincipal", (platform) =>
		platform.ensurePrincipal(session.user),
	);
	return platformCall(runtime, "createProject", (platform) =>
		platform.createProject(session.user, projectName),
	);
}

async function inspectRepositoryFromSession(
	runtime: DashboardRuntime,
	input: { repositorySelector: string },
): Promise<DashboardOnboardingDraft> {
	const session = await requireSession(runtime);
	const selector = normalizeRepositorySelector(input.repositorySelector);
	await platformCall(runtime, "ensurePrincipal", (platform) =>
		platform.ensurePrincipal(session.user),
	);
	const inspection = await platformCall(
		runtime,
		"inspectRepositorySource",
		(platform) =>
			platform.inspectRepositorySource(session.user, {
				provider: "github",
				repositorySelector: selector,
			}),
	);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
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
	return saveOnboardingDraft(runtime, session.user.id, nextDraft);
}

async function confirmRepositoryFromSession(
	runtime: DashboardRuntime,
	input: {
		repositorySelector: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		containerPort?: string;
	},
): Promise<DashboardOnboardingDraft> {
	const session = await requireSession(runtime);
	const selector = normalizeRepositorySelector(input.repositorySelector);
	await platformCall(runtime, "ensurePrincipal", (platform) =>
		platform.ensurePrincipal(session.user),
	);
	const inspection = await platformCall(
		runtime,
		"inspectRepositorySource",
		(platform) =>
			platform.inspectRepositorySource(session.user, {
				provider: "github",
				repositorySelector: selector,
			}),
	);
	if (inspection.accessState !== "available") {
		throw new DashboardValidationError({
			message: "Repository access is not available yet.",
		});
	}

	const dockerfilePath =
		input.dockerfilePath?.trim() ||
		inspection.recommendedBuildRecipe?.dockerfilePath ||
		"";
	if (dockerfilePath === "") {
		throw new DashboardValidationError({
			message:
				"No Dockerfile was detected for this repository. Pick a repo with a Dockerfile or add one first.",
		});
	}
	const contextDir =
		input.contextDir?.trim() ||
		inspection.recommendedBuildRecipe?.contextDir ||
		".";
	const containerPort = parseContainerPort(input.containerPort);
	const trackedRef = input.trackedRef?.trim() || inspection.defaultBranch || "main";
	const projects = await platformCall(runtime, "listProjects", (platform) =>
		platform.listProjects(session.user),
	);
	const projectName = selector;
	const project =
		projects.find((entry) => entry.name === projectName) ??
		(await platformCall(runtime, "createProject", (platform) =>
			platform.createProject(session.user, projectName),
		));
	const services = await platformCall(runtime, "listServices", (platform) =>
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
		? await platformCall(runtime, "updateService", (platform) =>
				platform.updateService(session.user, {
					projectId: project.id,
					serviceId: existingService.id,
					source: desiredSource,
				}),
			)
		: await platformCall(runtime, "createService", (platform) =>
				platform.createService(session.user, {
					projectId: project.id,
					name: nextServiceName(services, selector),
					source: desiredSource,
				}),
			);
	return saveOnboardingDraft(runtime, session.user.id, {
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
}

async function saveHostnameFromSession(
	runtime: DashboardRuntime,
	hostname: string,
): Promise<DashboardOnboardingDraft> {
	const session = await requireSession(runtime);
	const normalizedHostname = normalizeHostname(hostname);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
	if (!draft.projectId || !draft.serviceId) {
		throw new DashboardValidationError({
			message: "Create a service before connecting a domain.",
		});
	}
	return saveOnboardingDraft(runtime, session.user.id, {
		...draft,
		currentStep: "domain",
		hostname: normalizedHostname,
	});
}

async function publishDomainFromSession(
	runtime: DashboardRuntime,
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
	if (!draft.projectId || !draft.serviceId) {
		throw new DashboardValidationError({
			message: "Create a service before publishing a domain.",
		});
	}
	if (!draft.hostname) {
		throw new DashboardValidationError({
			message: "Enter a hostname first.",
		});
	}
	const serviceStatus = await platformCall(runtime, "getServiceStatus", (platform) =>
		platform.getServiceStatus(session.user, {
			projectId: draft.projectId,
			serviceId: draft.serviceId,
		}),
	);
	if (!buildHealthyAndReady(serviceStatus)) {
		throw new DashboardValidationError({
			message:
				"Wait for the latest build to succeed and the deployment to become healthy before publishing a domain.",
		});
	}
	const verification = await verifyHostnameOrThrow(runtime, draft.hostname);
	if (verification.state !== "verified") {
		throw new DashboardValidationError({
			message: "DNS has not verified yet for this hostname.",
		});
	}
	const binding = await platformCall(runtime, "createDomainBinding", (platform) =>
		platform.createDomainBinding(session.user, {
			projectId: draft.projectId,
			serviceId: draft.serviceId,
			hostname: draft.hostname,
		}),
	);
	await saveOnboardingDraft(runtime, session.user.id, {
		...draft,
		currentStep: "domain",
	});
	return binding;
}

async function clearSession(runtime: DashboardRuntime): Promise<void> {
	const { config, cookies } = runtime;
	const refreshToken = cookies.get(config.refreshCookieName);
	if (refreshToken) {
		const refresh = readRefreshToken(config, refreshToken);
		if (refresh) {
			await storeCall(runtime, "ensureInitialized", (store) =>
				store.ensureInitialized(),
			);
			await storeCall(runtime, "deleteRefreshSession", (store) =>
				store.deleteRefreshSession(refresh.sessionId),
			);
		}
	}
	clearAuthCookies(cookies, config);
}

async function signIn(
	runtime: DashboardRuntime,
	user: DashboardUser,
	redirectTo?: string,
): Promise<string> {
	const { config, cookies } = runtime;
	const now = runtime.now();
	const refreshSessionId = runtime.randomUUID();
	const tokens = createSessionTokenPair(config, user, refreshSessionId, now);
	await storeCall(runtime, "createRefreshSession", (store) =>
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
	await safePlatformCall(runtime, "ensurePrincipal", (platform) =>
		platform.ensurePrincipal(user),
	);
	return sanitizeRedirect(redirectTo);
}

async function currentSession(
	runtime: DashboardRuntime,
): Promise<DashboardSession | null> {
	const { config, cookies } = runtime;
	const now = runtime.now();
	const accessToken = cookies.get(config.sessionCookieName);
	if (accessToken) {
		const access = readAccessToken(config, accessToken, now);
		if (access) {
			return { user: access.user };
		}
	}
	return refreshSessionFromCookies(runtime);
}

async function refreshSession(runtime: DashboardRuntime): Promise<void> {
	const session = await refreshSessionFromCookies(runtime);
	if (!session) {
		throw new AuthenticationRequiredError({
			message: "authentication required",
		});
	}
}

async function requireSession(
	runtime: DashboardRuntime,
): Promise<DashboardSession> {
	const session = await currentSession(runtime);
	if (!session) {
		throw new AuthenticationRequiredError({
			message: "authentication required",
		});
	}
	return session;
}

async function loadOnboardingDraft(
	runtime: DashboardRuntime,
	userID: string,
): Promise<DashboardOnboardingDraft> {
	return storeCall(runtime, "getOnboardingDraft", (store) =>
		store.getOnboardingDraft(userID),
	);
}

async function saveOnboardingDraft(
	runtime: DashboardRuntime,
	userID: string,
	draft: DashboardOnboardingDraft,
): Promise<DashboardOnboardingDraft> {
	return storeCall(runtime, "saveOnboardingDraft", (store) =>
		store.saveOnboardingDraft(userID, draft),
	);
}

async function listGitHubRepositories(
	runtime: DashboardRuntime,
	accessToken: string,
): Promise<Array<GitHubUserRepository>> {
	const github = runtime.github;
	if (!github) {
		return [];
	}
	try {
		return await github.listRepositories(accessToken);
	} catch (cause) {
		if (cause instanceof GitHubApiError) {
			return [];
		}
		throw toGitHubApiError("listRepositories", cause);
	}
}

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

async function storeCall<A>(
	runtime: DashboardRuntime,
	operation: string,
	run: (store: DashboardStore) => Promise<A>,
): Promise<A> {
	try {
		return await run(runtime.store);
	} catch (cause) {
		throw toStrictDatabaseError(operation, cause);
	}
}

async function storeAuthCall<A>(
	runtime: DashboardRuntime,
	operation: string,
	run: (store: DashboardStore) => Promise<A>,
): Promise<A> {
	try {
		return await run(runtime.store);
	} catch (cause) {
		throw toDatabaseError(operation, cause);
	}
}

async function githubCall<A>(
	runtime: DashboardRuntime,
	operation: string,
	run: () => Promise<A>,
): Promise<A> {
	void runtime;
	try {
		return await run();
	} catch (cause) {
		throw toGitHubApiError(operation, cause);
	}
}

async function platformCall<A>(
	runtime: DashboardRuntime,
	operation: string,
	run: (platform: PlatformGateway) => Promise<A>,
): Promise<A> {
	try {
		return await run(runtime.platform);
	} catch (cause) {
		throw toPlatformGatewayError(operation, cause);
	}
}

async function safePlatformCall<A>(
	runtime: DashboardRuntime,
	operation: string,
	run: (platform: PlatformGateway) => Promise<A>,
): Promise<A | undefined> {
	try {
		return await platformCall(runtime, operation, run);
	} catch (error) {
		if (error instanceof PlatformGatewayError) {
			return undefined;
		}
		throw error;
	}
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

async function refreshSessionFromCookies(
	runtime: DashboardRuntime,
): Promise<DashboardSession | null> {
	const { config, cookies } = runtime;
	const now = runtime.now();
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

	await storeCall(runtime, "ensureInitialized", (store) =>
		store.ensureInitialized(),
	);
	const nextSessionId = runtime.randomUUID();
	const nextRefreshExpiresAt = new Date(
		now.getTime() + config.sessionMaxAgeSeconds * 1000,
	);
	const user = await storeCall(runtime, "rotateRefreshSession", (store) =>
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
}

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

function authCallbackURL(config: DashboardConfig): string {
	return `${config.publicBaseURL.replace(/\/$/, "")}/auth/callback`;
}

function parseAuthStateCookie(rawState: string): AuthStateCookie {
	try {
		const parsed = JSON.parse(rawState);
		if (
			parsed &&
			typeof parsed === "object" &&
			typeof parsed.state === "string" &&
			parsed.state.trim() !== "" &&
			typeof parsed.redirectTo === "string"
		) {
			return {
				state: parsed.state,
				redirectTo: parsed.redirectTo,
			};
		}
	} catch {
		// Fall through.
	}
	throw new AuthConflictError({
		code: "invalid_signin_state",
		message: "invalid sign-in state cookie",
	});
}

async function verifyHostnameOrThrow(
	runtime: DashboardRuntime,
	hostname: string,
): Promise<DomainVerificationResult> {
	try {
		return await verifyHostnameDNS(
			hostname,
			runtime.config.ingressTargetHost,
			undefined,
			runtime.config.localDomainSuffix,
		);
	} catch (cause) {
		throw new DashboardValidationError({
			message: formatError(cause),
		});
	}
}

async function safeVerifyHostname(
	runtime: DashboardRuntime,
	hostname: string,
): Promise<DomainVerificationResult | undefined> {
	try {
		return await verifyHostnameOrThrow(runtime, hostname);
	} catch (error) {
		if (error instanceof DashboardValidationError) {
			return undefined;
		}
		throw error;
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
	if (/^[A-Za-z_][A-Za-z0-9_]*$/.test(raw)) {
		return raw;
	}
	throw new DashboardConfigError({
		message: `invalid dashboard schema identifier: ${raw}`,
	});
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
			const parsed = {
				subject: (subject ?? "").trim(),
				email: (email ?? "").trim(),
			};
			if (parsed.subject === "" || parsed.email === "") {
				return [];
			}
			return [parsed];
		});
}
