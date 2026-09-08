import {
	AuthConflictError,
	type DashboardConfig,
	type DashboardDependencies,
	type DashboardOnboardingDraft,
	type DashboardProject,
	type DashboardServiceRecord,
	type DashboardStore,
	DashboardValidationError,
	DatabaseError,
	GitHubApiError,
	type GitHubAppUserClient,
	type GitHubUserRepository,
	type PlatformGateway,
	PlatformGatewayError,
	type SessionCookies,
	type StoredDashboardGitHubAccount,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/dashboard/core/utils.server";
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
	userID: string,
	account: StoredDashboardGitHubAccount,
): Promise<StoredDashboardGitHubAccount> {
	const github = runtime.github;
	if (!github || !account.refreshToken) {
		throw new GitHubApiError({
			operation: "refreshToken",
			message: "GitHub token refresh is unavailable",
			cause: new Error("missing GitHub client or refresh token"),
		});
	}
	const expectedTokenVersion = account.tokenVersion;
	const leaseID = runtime.randomUUID();
	for (let attempt = 0; attempt < 80; attempt += 1) {
		const current =
			attempt === 0
				? account
				: await storeCall(runtime, "getGitHubAccount", (store) =>
						store.getGitHubAccount(userID),
					);
		if (!current) {
			throw new GitHubApiError({
				operation: "refreshToken",
				message: "GitHub account is no longer available",
				cause: new Error("GitHub account missing during token refresh"),
			});
		}
		if (current.tokenVersion !== expectedTokenVersion) return current;

		const now = runtime.now();
		const acquired = await storeCall(
			runtime,
			"tryAcquireGitHubTokenRefresh",
			(store) =>
				store.tryAcquireGitHubTokenRefresh({
					userID,
					expectedTokenVersion,
					leaseID,
					now,
					leaseExpiresAt: new Date(now.getTime() + 60_000),
				}),
		);
		if (!acquired) {
			await waitForTokenRefresh();
			continue;
		}

		try {
			const token = await github.refreshToken(account.refreshToken);
			const updated = await storeCall(
				runtime,
				"completeGitHubTokenRefresh",
				(store) =>
					store.completeGitHubTokenRefresh({
						userID,
						providerSubject: account.providerSubject,
						expectedTokenVersion,
						leaseID,
						token,
						fallbackRefreshToken: account.refreshToken ?? "",
						fallbackRefreshTokenExpiresAt: account.refreshTokenExpiresAt,
					}),
			);
			if (updated) return updated;
			const winner = await storeCall(runtime, "getGitHubAccount", (store) =>
				store.getGitHubAccount(userID),
			);
			if (winner && winner.tokenVersion !== expectedTokenVersion) return winner;
			throw new GitHubApiError({
				operation: "refreshToken",
				message: "GitHub token refresh was superseded",
				cause: new Error("stale GitHub token refresh result"),
			});
		} catch (cause) {
			await storeCall(runtime, "releaseGitHubTokenRefresh", (store) =>
				store.releaseGitHubTokenRefresh({
					userID,
					expectedTokenVersion,
					leaseID,
				}),
			);
			const winner = await storeCall(runtime, "getGitHubAccount", (store) =>
				store.getGitHubAccount(userID),
			);
			if (winner && winner.tokenVersion !== expectedTokenVersion) return winner;
			throw cause;
		}
	}

	const winner = await storeCall(runtime, "getGitHubAccount", (store) =>
		store.getGitHubAccount(userID),
	);
	if (winner && winner.tokenVersion !== expectedTokenVersion) return winner;
	throw new GitHubApiError({
		operation: "refreshToken",
		message: "GitHub token refresh is already in progress",
		cause: new Error("GitHub token refresh lease wait timed out"),
	});
}

function waitForTokenRefresh(): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, 25));
}

export function reconcileOnboardingDraft(
	draft: DashboardOnboardingDraft,
	project: DashboardProject | undefined,
	service: DashboardServiceRecord | undefined,
): DashboardOnboardingDraft {
	const source = service?.spec?.source;
	return {
		...defaultOnboardingDraft(),
		...draft,
		projectId: project?.id ?? "",
		serviceId: service?.id ?? "",
		repositorySelector:
			draft.repositorySelector || source?.repositorySelector || "",
		trackedRef: draft.trackedRef || source?.trackedRef || "",
		dockerfilePath:
			draft.dockerfilePath || source?.buildRecipe?.dockerfilePath || "",
		contextDir: draft.contextDir || source?.buildRecipe?.contextDir || "",
		hostname: draft.hostname,
	};
}

export function onboardingDraftEquals(
	left: DashboardOnboardingDraft,
	right: DashboardOnboardingDraft,
): boolean {
	return (
		left.projectId === right.projectId &&
		left.environmentId === right.environmentId &&
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

// The single owner of store-initialization caching: successful initialization
// is shared, and a failure drops the promise so the next use retries.
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
	operation: string,
	run: () => Promise<A>,
): Promise<A> {
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
