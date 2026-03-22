import { Buffer } from "node:buffer";
import { randomUUID } from "node:crypto";
import {
	deleteCookie,
	getCookie,
	setCookie,
} from "@tanstack/react-start/server";
import { Effect, Layer, ManagedRuntime, Schema, ServiceMap } from "effect";
import { Pool } from "pg";

import {
	DashboardClockService,
	DashboardConfigError,
	DashboardConfigService,
	DashboardStoreService,
	DashboardUUIDService,
	GitHubAppUserClientService,
	PlatformGatewayService,
	createDashboardRequestLayer,
	type DashboardConfig,
	type DashboardHomeState,
	type DashboardProject,
	type DevLoginIdentity,
	type SessionCookiesService,
	beginGitHubLoginEffect,
	clearSessionEffect,
	completeAuthCallbackEffect,
	createProjectFromSessionEffect,
	confirmRepositoryFromSessionEffect,
	isGitHubLoginEnabledEffect,
	inspectRepositoryFromSessionEffect,
	listDevLoginsEffect,
	loadDashboardHomeEffect,
	parseDevUsers,
	parseIdentifier,
	publishDomainFromSessionEffect,
	refreshSessionEffect,
	saveHostnameFromSessionEffect,
} from "#/lib/dashboard-core.server";
import { createPostgresDashboardStore } from "#/lib/dashboard-store.server";
import { createGitHubAppUserClient } from "#/lib/github-auth.server";
import {
	createPlatformGateway,
	ingestGitHubWebhook,
	type IngestGitHubWebhookInput,
} from "#/lib/platform-grpc.server";

interface RuntimeConfig extends DashboardConfig {
	databaseURL: string;
	databaseSchema: string;
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
}

class PostgresPoolService extends ServiceMap.Service<
	PostgresPoolService,
	Pool
>()("dashboard/PostgresPool") {}

type ServerServices =
	| DashboardConfigService
	| DashboardStoreService
	| PlatformGatewayService
	| DashboardClockService
	| DashboardUUIDService
	| GitHubAppUserClientService
	| PostgresPoolService;

type RequestServices = ServerServices | SessionCookiesService;

const NonEmptyEnvString = Schema.NonEmptyString;
const Base64EnvString = Schema.String.check(Schema.isBase64());
const decodeNonEmptyEnv = Schema.decodeUnknownSync(NonEmptyEnvString);
const decodeBase64EnvString = Schema.decodeUnknownSync(Base64EnvString);

const config = readConfig();
const runtime = ManagedRuntime.make(createServerLayer(config));

export function listDevLogins(): Array<DevLoginIdentity> {
	return runtime.runSync(listDevLoginsEffect);
}

export function isGitHubLoginEnabled(): boolean {
	return runtime.runSync(isGitHubLoginEnabledEffect);
}

export function getPublicBaseURL(): string {
	return config.publicBaseURL;
}

export function beginGitHubLogin(input: {
	redirectTo?: string;
}): Promise<string> {
	return runRequest("beginGitHubLogin", beginGitHubLoginEffect(input));
}

export function completeAuthCallback(input: {
	code?: string;
	state?: string;
	subject?: string;
	email?: string;
	redirectTo?: string;
}): Promise<string> {
	return runRequest("completeAuthCallback", completeAuthCallbackEffect(input));
}

export function loadDashboardHome(): Promise<DashboardHomeState | null> {
	return runRequest("loadDashboardHome", loadDashboardHomeEffect);
}

export function createProjectFromSession(
	name: string,
): Promise<DashboardProject> {
	return runRequest(
		"createProjectFromSession",
		createProjectFromSessionEffect(name),
	);
}

export function inspectRepositoryFromSession(input: {
	repositorySelector: string;
}): Promise<import("#/lib/dashboard-core.server").DashboardOnboardingDraft> {
	return runRequest(
		"inspectRepositoryFromSession",
		inspectRepositoryFromSessionEffect(input),
	);
}

export function confirmRepositoryFromSession(input: {
	repositorySelector: string;
	trackedRef?: string;
	dockerfilePath?: string;
	contextDir?: string;
	containerPort?: string;
}): Promise<import("#/lib/dashboard-core.server").DashboardOnboardingDraft> {
	return runRequest(
		"confirmRepositoryFromSession",
		confirmRepositoryFromSessionEffect(input),
	);
}

export function saveHostnameFromSession(
	hostname: string,
): Promise<import("#/lib/dashboard-core.server").DashboardOnboardingDraft> {
	return runRequest(
		"saveHostnameFromSession",
		saveHostnameFromSessionEffect(hostname),
	);
}

export function publishDomainFromSession(): Promise<
	import("#/lib/dashboard-core.server").DashboardDomainBinding
> {
	return runRequest("publishDomainFromSession", publishDomainFromSessionEffect);
}

export function clearSession(): Promise<void> {
	return runRequest("clearSession", clearSessionEffect);
}

export function refreshSession(): Promise<void> {
	return runRequest("refreshSession", refreshSessionEffect);
}

export function forwardGitHubWebhook(
	input: IngestGitHubWebhookInput,
): Promise<void> {
	return runtime.runPromise(
		Effect.tryPromise({
			try: () => ingestGitHubWebhook(config, input),
			catch: (cause) => cause,
		}).pipe(Effect.withSpan("dashboard.forwardGitHubWebhook")),
	);
}

function createServerLayer(config: RuntimeConfig): Layer.Layer<ServerServices> {
	const poolLayer = Layer.effect(PostgresPoolService)(
		Effect.acquireRelease(
			Effect.sync(
				() =>
					new Pool({
						connectionString: config.databaseURL,
					}),
			),
			(pool) => Effect.promise(() => pool.end()).pipe(Effect.asVoid),
		),
	);

	const storeLayer = Layer.effect(DashboardStoreService)(
		Effect.gen(function* () {
			const pool = yield* PostgresPoolService;
			return createPostgresDashboardStore(config, pool);
		}),
	).pipe(Layer.provide(poolLayer));

	let layer = Layer.mergeAll(
		poolLayer,
		Layer.succeed(DashboardConfigService)(config),
		storeLayer,
		Layer.succeed(PlatformGatewayService)(createPlatformGateway(config)),
		Layer.succeed(DashboardClockService)({
			now: () => new Date(),
		}),
		Layer.succeed(DashboardUUIDService)({
			randomUUID,
		}),
	);

	if (config.github) {
		layer = Layer.merge(
			layer,
			Layer.succeed(GitHubAppUserClientService)(
				createGitHubAppUserClient(config.github),
			),
		);
	}

	return layer as unknown as Layer.Layer<ServerServices>;
}

function runRequest<A, E>(
	operation: string,
	program: Effect.Effect<A, E, RequestServices>,
): Promise<A> {
	void operation;
	return runtime.runPromise(
		program.pipe(Effect.provide(requestCookiesLayer())),
	);
}

function requestCookiesLayer() {
	return createDashboardRequestLayer({
		get: (name) => getCookie(name) ?? undefined,
		set: (name, value, options) => setCookie(name, value, options),
		delete: (name, options) => deleteCookie(name, options),
	});
}

function readConfig(): RuntimeConfig {
	const databaseURL = mustEnv("DASHBOARD_DATABASE_URL");
	const databaseSchema = parseIdentifier(
		process.env.DASHBOARD_DATABASE_SCHEMA ?? "dashboard",
	);
	const sessionCookieName =
		process.env.DASHBOARD_SESSION_COOKIE_NAME ?? "dashboard_session";
	const refreshCookieName =
		process.env.DASHBOARD_REFRESH_COOKIE_NAME ?? `${sessionCookieName}_refresh`;
	const publicBaseURL =
		process.env.DASHBOARD_PUBLIC_BASE_URL ?? "http://localhost:3000";
	const localIngressBaseURL =
		process.env.DASHBOARD_LOCAL_INGRESS_BASE_URL?.trim() || undefined;

	return {
		databaseURL,
		databaseSchema,
		sessionCookieName,
		refreshCookieName,
		authStateCookieName:
			process.env.DASHBOARD_AUTH_STATE_COOKIE_NAME ??
			process.env.DASHBOARD_OAUTH_STATE_COOKIE_NAME ??
			"dashboard_auth_state",
		publicBaseURL,
		localIngressBaseURL,
		githubInstallURL:
			process.env.DASHBOARD_GITHUB_INSTALL_URL?.trim() || undefined,
		ingressTargetHost: mustEnv("DASHBOARD_INGRESS_TARGET_HOST"),
		localDomainSuffix:
			process.env.DASHBOARD_LOCAL_DOMAIN_SUFFIX?.trim() || undefined,
		controlPlaneAddress: mustEnv("DASHBOARD_CONTROLPLANE_ADDRESS"),
		controlPlaneServerName:
			process.env.DASHBOARD_CONTROLPLANE_SERVER_NAME ?? "controlplane",
		jwtSecret: mustEnv("DASHBOARD_JWT_SECRET"),
		controlPlaneCA: decodeBase64Env("DASHBOARD_CONTROLPLANE_CA_PEM_B64"),
		controlPlaneCert: decodeBase64Env("DASHBOARD_CONTROLPLANE_CERT_PEM_B64"),
		controlPlaneKey: decodeBase64Env("DASHBOARD_CONTROLPLANE_KEY_PEM_B64"),
		devUsers: parseDevUsers(process.env.DASHBOARD_DEV_USERS ?? ""),
		sessionMaxAgeSeconds: 30 * 24 * 60 * 60,
		github:
			process.env.DASHBOARD_GITHUB_CLIENT_ID &&
			process.env.DASHBOARD_GITHUB_CLIENT_SECRET &&
			process.env.DASHBOARD_GITHUB_APP_ID
				? {
						appId: mustEnv("DASHBOARD_GITHUB_APP_ID"),
						clientId: mustEnv("DASHBOARD_GITHUB_CLIENT_ID"),
						clientSecret: mustEnv("DASHBOARD_GITHUB_CLIENT_SECRET"),
						authorizationBaseURL:
							process.env.DASHBOARD_GITHUB_AUTH_BASE_URL ??
							"https://github.com",
						apiBaseURL:
							process.env.DASHBOARD_GITHUB_API_BASE_URL ??
							"https://api.github.com",
					}
				: undefined,
	};
}

function decodeBase64Env(name: string): Buffer {
	try {
		return Buffer.from(decodeBase64EnvString(mustEnv(name)), "base64");
	} catch {
		throw new DashboardConfigError({
			message: `invalid required base64 environment variable ${name}`,
		});
	}
}

function mustEnv(name: string): string {
	const value = process.env[name];
	try {
		return decodeNonEmptyEnv(value?.trim());
	} catch {
		throw new DashboardConfigError({
			message: `missing required environment variable ${name}`,
		});
	}
}

export type {
	DashboardConfig,
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardOnboardingDraft,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardServiceStatus,
	DevLoginIdentity,
} from "#/lib/dashboard-core.server";
