import {
	AuthConflictError,
	type DashboardConfig,
	type DashboardDependencies,
	type DashboardDomainBinding,
	type DashboardGitHubAccount,
	type DashboardOnboardingDraft,
	type DashboardOnboardingStep,
	type DashboardProject,
	type DashboardRepositoryInspection,
	type DashboardRuntimePort,
	type DashboardServiceRecord,
	type DashboardServiceStatus,
	type DashboardStore,
	DashboardValidationError,
	DatabaseError,
	type GitHubAccountLoginInput,
	GitHubApiError,
	type GitHubAppUserClient,
	type GitHubUserRepository,
	type PlatformGateway,
	PlatformGatewayError,
	type SessionCookies,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/dashboard/core/utils.server";
import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";
import { verifyHostnameDNS } from "#/lib/dashboard/domain/dns.server";
import {
	defaultOnboardingDraft,
	generatedServiceNameFromSeed,
	uniqueServiceName,
} from "#/lib/dashboard/onboarding/flow";

export interface DashboardRuntime {
	config: DashboardConfig;
	store: DashboardStore;
	storeInitPromise?: Promise<void>;
	platform: PlatformGateway;
	cookies: SessionCookies;
	github?: GitHubAppUserClient;
	now: () => Date;
	randomUUID: () => string;
}

export function createDashboardRuntime(
	config: DashboardConfig,
	deps: DashboardDependencies,
): DashboardRuntime {
	return {
		config,
		store: deps.store,
		storeInitPromise: undefined,
		platform: deps.platform,
		cookies: deps.cookies,
		github: deps.github,
		now: deps.now ?? (() => new Date()),
		randomUUID: deps.randomUUID ?? (() => crypto.randomUUID()),
	};
}

export async function loadOnboardingDraft(
	runtime: DashboardRuntime,
	userID: string,
): Promise<DashboardOnboardingDraft> {
	return storeCall(runtime, "getOnboardingDraft", (store) =>
		store.getOnboardingDraft(userID),
	);
}

export async function saveOnboardingDraft(
	runtime: DashboardRuntime,
	userID: string,
	draft: DashboardOnboardingDraft,
): Promise<DashboardOnboardingDraft> {
	return storeCall(runtime, "saveOnboardingDraft", (store) =>
		store.saveOnboardingDraft(userID, draft),
	);
}

export async function listGitHubRepositories(
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
		throw toGitHubApiError("listRepositories", cause);
	}
}

export async function refreshGitHubAccount(
	runtime: DashboardRuntime,
	account: DashboardGitHubAccount,
): Promise<DashboardGitHubAccount> {
	const github = runtime.github;
	if (!github || !account.refreshToken) {
		throw new GitHubApiError({
			operation: "refreshToken",
			message: "GitHub token refresh is unavailable",
			cause: new Error("missing GitHub client or refresh token"),
		});
	}
	const token = await github.refreshToken(account.refreshToken);
	const refreshed: GitHubAccountLoginInput = {
		providerSubject: account.providerSubject,
		login: account.login,
		primaryEmail: account.primaryEmail,
		accessToken: token.accessToken,
		tokenType: token.tokenType,
		scope: token.scope,
		accessTokenExpiresAt: token.accessTokenExpiresAt,
		refreshToken: token.refreshToken ?? account.refreshToken,
		refreshTokenExpiresAt:
			token.refreshTokenExpiresAt ?? account.refreshTokenExpiresAt,
	};
	await storeCall(runtime, "completeGitHubLogin", (store) =>
		store.completeGitHubLogin(refreshed),
	);
	return {
		...account,
		accessToken: refreshed.accessToken,
		tokenType: refreshed.tokenType,
		scope: refreshed.scope,
		accessTokenExpiresAt: refreshed.accessTokenExpiresAt,
		refreshToken: refreshed.refreshToken,
		refreshTokenExpiresAt: refreshed.refreshTokenExpiresAt,
	};
}

export function reconcileOnboardingDraft(
	draft: DashboardOnboardingDraft,
	inspection: DashboardRepositoryInspection | undefined,
	project: DashboardProject | undefined,
	service: DashboardServiceRecord | undefined,
	serviceStatus: DashboardServiceStatus | undefined,
	domainBindings: Array<DashboardDomainBinding>,
): DashboardOnboardingDraft {
	const source = service?.spec?.source;
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

export function onboardingDraftEquals(
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
		left.hostname === right.hostname
	);
}

export function parseTargetPort(raw: string | number | undefined): number {
	const value = typeof raw === "number" ? String(raw) : (raw?.trim() ?? "");
	if (value === "") {
		return 8080;
	}
	const port = /^\d+$/.test(value) ? Number(value) : Number.NaN;
	if (!Number.isInteger(port) || port < 1 || port > 65535) {
		throw new DashboardValidationError({
			message: "App port must be an integer between 1 and 65535.",
		});
	}
	return port;
}

export function recommendedTargetPort(
	service: DashboardServiceRecord | undefined,
	status: DashboardServiceStatus | undefined,
): number {
	for (const port of sortedRuntimePorts(service?.spec?.runtime.ports ?? [])) {
		if (port.primary) {
			return port.port;
		}
	}
	for (const port of status?.allocation?.healthyPorts ?? []) {
		if (Number.isInteger(port) && port >= 1 && port <= 65535) {
			return port;
		}
	}
	return 8080;
}

function sortedRuntimePorts(
	ports: DashboardRuntimePort[],
): DashboardRuntimePort[] {
	return [...ports].sort((left, right) => {
		if (left.primary !== right.primary) {
			return left.primary ? -1 : 1;
		}
		return left.port - right.port;
	});
}

export function nextGeneratedServiceName(
	services: Array<DashboardServiceRecord>,
	preferredName: string | undefined,
	seed: string,
): string {
	return uniqueServiceName(
		services.map((service) => service.name),
		preferredName?.trim() || generatedServiceNameFromSeed(seed),
	);
}

export function nextGeneratedProjectName(
	projects: Array<DashboardProject>,
	seed: string,
): string {
	return uniqueServiceName(
		projects.map((project) => project.name),
		generatedServiceNameFromSeed(seed),
	);
}

export function buildHealthyAndReady(
	status: DashboardServiceStatus | undefined,
): boolean {
	return (
		status?.service.latestBuild?.state === "succeeded" &&
		status.allocation?.healthy === true
	);
}

export async function storeCall<A>(
	runtime: DashboardRuntime,
	operation: string,
	run: (store: DashboardStore) => Promise<A>,
): Promise<A> {
	try {
		await ensureStoreInitialized(runtime);
		return await run(runtime.store);
	} catch (cause) {
		throw toStrictDatabaseError(operation, cause);
	}
}

export async function storeAuthCall<A>(
	runtime: DashboardRuntime,
	operation: string,
	run: (store: DashboardStore) => Promise<A>,
): Promise<A> {
	try {
		await ensureStoreInitialized(runtime);
		return await run(runtime.store);
	} catch (cause) {
		throw toDatabaseError(operation, cause);
	}
}

async function ensureStoreInitialized(
	runtime: DashboardRuntime,
): Promise<void> {
	if (!runtime.storeInitPromise) {
		runtime.storeInitPromise = runtime.store
			.ensureInitialized()
			.catch((cause) => {
				runtime.storeInitPromise = undefined;
				throw cause;
			});
	}
	await runtime.storeInitPromise;
}

export async function githubCall<A>(
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

export async function platformCall<A>(
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

export async function safePlatformCall<A>(
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

export function toGitHubApiError(
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

export async function verifyHostnameOrThrow(
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

export async function safeVerifyHostname(
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
