import { Buffer } from "node:buffer";
import { randomUUID } from "node:crypto";
import { readFileSync, statSync } from "node:fs";
import {
	deleteCookie,
	getCookie,
	setCookie,
} from "@tanstack/react-start/server";
import { Pool } from "pg";
import {
	assertDistinctDashboardSecrets,
	assertProductionDashboardConfig,
	formatDashboardStartupContract,
	parseRuntimeProfile,
	usesSecureCookies,
} from "#/lib/dashboard/core/profile.server";
import {
	createDashboardRuntime,
	type DashboardRuntime,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardConfig,
	DashboardConfigError,
	type DevLoginIdentity,
} from "#/lib/dashboard/core/types.server";
import {
	parseDevUsers,
	parseIdentifier,
} from "#/lib/dashboard/core/utils.server";
import { createGitHubAppUserClient } from "#/lib/dashboard/github/auth.server";
import { createPostgresDashboardStore } from "#/lib/dashboard/store/postgres.server";
import type { GitHubTokenCipher } from "#/lib/dashboard/store/token-crypto.server";
import {
	createGitHubTokenCipher,
	decodeGitHubTokenEncryptionKey,
} from "#/lib/dashboard/store/token-crypto.server";
import {
	createPlatformGateway,
	type IngestGitHubWebhookInput,
	ingestGitHubWebhook,
} from "#/lib/platform-grpc/gateway.server";

interface RuntimeConfig extends DashboardConfig {
	databaseURL: string;
	databaseSchema: string;
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
	userAssertionSecret: string;
	githubTokenCipher: GitHubTokenCipher;
}

let config: RuntimeConfig | undefined;
let pool: Pool | undefined;
let dashboardRuntime: DashboardRuntime | undefined;

export function getDashboardRuntime(): DashboardRuntime {
	const config = getConfig();
	dashboardRuntime ??= createDashboardRuntime(config, {
		store: createPostgresDashboardStore(config, getPool()),
		platform: createPlatformGateway(config),
		github: config.github
			? createGitHubAppUserClient(config.github)
			: undefined,
		cookies: {
			get: (name) => getCookie(name) ?? undefined,
			set: (name, value, options) => setCookie(name, value, options),
			delete: (name, options) => deleteCookie(name, options),
		},
		now: () => new Date(),
		randomUUID,
	});
	return dashboardRuntime;
}

function getConfig(): RuntimeConfig {
	config ??= readConfig();
	return config;
}

function getPool(): Pool {
	const current = getConfig();
	pool ??= new Pool({
		connectionString: current.databaseURL,
		max: 2,
		idleTimeoutMillis: 30_000,
	});
	return pool;
}

export async function checkDashboardReadiness(): Promise<{
	status: "ready" | "not_ready";
	failed?: Array<string>;
}> {
	try {
		getConfig();
	} catch {
		return { status: "not_ready", failed: ["configuration"] };
	}
	try {
		await getPool().query("SELECT 1");
	} catch {
		return { status: "not_ready", failed: ["database"] };
	}
	try {
		await createPostgresDashboardStore(
			getConfig(),
			getPool(),
		).ensureInitialized();
	} catch {
		return { status: "not_ready", failed: ["migrations"] };
	}
	return { status: "ready" };
}

export function listDevLogins(): Array<DevLoginIdentity> {
	return getConfig().devUsers;
}

export function isGitHubLoginEnabled(): boolean {
	return Boolean(getConfig().github);
}

export function getPublicBaseURL(): string {
	return getConfig().publicBaseURL;
}

export function forwardGitHubWebhook(
	input: IngestGitHubWebhookInput,
): Promise<void> {
	return ingestGitHubWebhook(getConfig(), input);
}

function readConfig(): RuntimeConfig {
	const databaseURL = mustSecret("DASHBOARD_DATABASE_URL");
	const githubClientSecret = optionalSecret("DASHBOARD_GITHUB_CLIENT_SECRET");
	const jwtSecret = mustSecret("DASHBOARD_JWT_SECRET");
	const jwtSecretPrevious = optionalSecret("DASHBOARD_JWT_SECRET_PREVIOUS");
	const userAssertionSecret = mustSecret(
		"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET",
	);
	const githubTokenEncryptionKeyValue = mustSecret(
		"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY",
	);
	let githubTokenEncryptionKey: Buffer;
	try {
		githubTokenEncryptionKey = decodeGitHubTokenEncryptionKey(
			githubTokenEncryptionKeyValue,
		);
	} catch {
		throw new DashboardConfigError({
			message:
				"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY must be base64 or base64url encoding of exactly 32 bytes",
		});
	}
	assertDistinctDashboardSecrets({
		jwtSecret,
		jwtSecretPrevious,
		userAssertionSecret,
		githubTokenEncryptionKeyValue,
		githubTokenEncryptionKey,
	});
	const profile = parseRuntimeProfile(process.env.DASHBOARD_PROFILE);
	const databaseSchema = parseIdentifier(
		process.env.DASHBOARD_DATABASE_SCHEMA ?? "dashboard",
	);
	const sessionCookieName =
		process.env.DASHBOARD_SESSION_COOKIE_NAME ?? "dashboard_session";
	const refreshCookieName =
		process.env.DASHBOARD_REFRESH_COOKIE_NAME ?? `${sessionCookieName}_refresh`;
	const publicBaseURL =
		process.env.DASHBOARD_PUBLIC_BASE_URL?.trim() ||
		(profile === "development" ? "http://localhost:3000" : "");
	const localIngressBaseURL =
		process.env.DASHBOARD_LOCAL_INGRESS_BASE_URL?.trim() || undefined;
	const controlPlaneAddress = mustEnv("DASHBOARD_CONTROLPLANE_ADDRESS");
	const controlPlaneServerName =
		process.env.DASHBOARD_CONTROLPLANE_SERVER_NAME ?? "controlplane";
	const devUsers = parseDevUsers(process.env.DASHBOARD_DEV_USERS ?? "");
	if (profile === "production") {
		assertProductionDashboardConfig({
			devUsers,
			publicBaseURL,
			localDomainSuffix: process.env.DASHBOARD_LOCAL_DOMAIN_SUFFIX?.trim(),
			localIngressBaseURL,
			jwtSecret,
			jwtSecretPrevious,
			userAssertionSecret,
			databaseURL,
			controlPlaneAddress,
			controlPlaneServerName,
		});
	}

	const admission = readConsoleAuthority(profile);
	const loaded = {
		installationID: admission?.installationId,
		recoveryGeneration: admission?.generation,
		recoveryPaused: admission?.paused ?? false,
		profile,
		databaseURL,
		databaseSchema,
		sessionCookieName,
		refreshCookieName,
		authStateCookieName:
			process.env.DASHBOARD_AUTH_STATE_COOKIE_NAME ?? "dashboard_auth_state",
		publicBaseURL,
		localIngressBaseURL,
		githubInstallURL:
			process.env.DASHBOARD_GITHUB_INSTALL_URL?.trim() || undefined,
		operatorGitHubLogin:
			process.env.DASHBOARD_OPERATOR_GITHUB_LOGIN?.trim().toLowerCase() ||
			undefined,
		ingressTargetHost: mustEnv("DASHBOARD_INGRESS_TARGET_HOST"),
		localDomainSuffix:
			process.env.DASHBOARD_LOCAL_DOMAIN_SUFFIX?.trim() || undefined,
		controlPlaneAddress,
		controlPlaneServerName,
		jwtSecret,
		jwtSecretPrevious,
		userAssertionSecret,
		githubTokenCipher: createGitHubTokenCipher(githubTokenEncryptionKey),
		controlPlaneCA: readPEM(
			"DASHBOARD_CONTROLPLANE_CA_PEM_B64",
			"DASHBOARD_CONTROLPLANE_CA_FILE",
		),
		controlPlaneCert: readPEM(
			"DASHBOARD_CONTROLPLANE_CERT_PEM_B64",
			"DASHBOARD_CONTROLPLANE_CERT_FILE",
		),
		controlPlaneKey: readPEM(
			"DASHBOARD_CONTROLPLANE_KEY_PEM_B64",
			"DASHBOARD_CONTROLPLANE_KEY_FILE",
		),
		devUsers,
		sessionMaxAgeSeconds: 30 * 24 * 60 * 60,
		github:
			process.env.DASHBOARD_GITHUB_CLIENT_ID &&
			githubClientSecret &&
			process.env.DASHBOARD_GITHUB_APP_ID
				? {
						appId: mustEnv("DASHBOARD_GITHUB_APP_ID"),
						clientId: mustEnv("DASHBOARD_GITHUB_CLIENT_ID"),
						clientSecret: githubClientSecret,
						authorizationBaseURL:
							process.env.DASHBOARD_GITHUB_AUTH_BASE_URL ??
							"https://github.com",
						apiBaseURL:
							process.env.DASHBOARD_GITHUB_API_BASE_URL ??
							"https://api.github.com",
					}
				: undefined,
	};
	console.info(
		formatDashboardStartupContract({
			profile: loaded.profile,
			githubEnabled: Boolean(loaded.github),
			secureCookies: usesSecureCookies(loaded.publicBaseURL),
			databaseURL: loaded.databaseURL,
			controlPlaneAddress: loaded.controlPlaneAddress,
		}),
	);
	return loaded;
}

function decodeBase64Env(name: string): Buffer {
	const value = mustEnv(name);
	if (
		!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
			value,
		)
	) {
		throw new DashboardConfigError({
			message: `invalid required base64 environment variable ${name}`,
		});
	}
	return Buffer.from(value, "base64");
}

function readPEM(base64EnvName: string, fileEnvName: string): Buffer {
	const fileName = process.env[fileEnvName]?.trim();
	if (fileName) {
		try {
			return readFileSync(fileName);
		} catch {
			throw new DashboardConfigError({
				message: `cannot read required secret file ${fileEnvName}`,
			});
		}
	}
	return decodeBase64Env(base64EnvName);
}

function mustSecret(name: string): string {
	const value = optionalSecret(name);
	if (value) return value;
	const fileEnvName = `${name}_FILE`;
	throw new DashboardConfigError({
		message: `missing required environment variable ${name} or ${fileEnvName}`,
	});
}

function optionalSecret(name: string): string | undefined {
	const value = process.env[name]?.trim();
	if (value) return value;
	const fileEnvName = `${name}_FILE`;
	const fileName = process.env[fileEnvName]?.trim();
	if (!fileName) return undefined;
	try {
		const secret = readFileSync(fileName, "utf8").trim();
		if (!secret) {
			throw new Error("secret file is empty");
		}
		return secret;
	} catch {
		throw new DashboardConfigError({
			message: `cannot read required secret file ${fileEnvName}`,
		});
	}
}

function mustEnv(name: string): string {
	const value = process.env[name]?.trim();
	if (!value) {
		throw new DashboardConfigError({
			message: `missing required environment variable ${name}`,
		});
	}
	return value;
}

function readConsoleAuthority(
	profile: "development" | "production",
): { installationId: string; generation: string; paused: boolean } | undefined {
	const file = process.env.DASHBOARD_AUTHORITY_FILE;
	if (!file && profile === "development") return undefined;
	if (!file?.startsWith("/"))
		throw new Error(
			"production console requires an absolute host-admin authority file",
		);
	const info = statSync(file);
	if (!info.isFile() || (info.mode & 0o077) !== 0)
		throw new Error("console authority file must be private");
	const authority = JSON.parse(readFileSync(file, "utf8"));
	if (
		typeof authority.installationId !== "string" ||
		!authority.installationId ||
		typeof authority.generation !== "string" ||
		!authority.generation ||
		typeof authority.paused !== "boolean"
	)
		throw new Error(
			"console admission requires installation, generation and pause state",
		);
	return authority;
}
