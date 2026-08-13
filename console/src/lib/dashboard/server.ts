import { Buffer } from "node:buffer";
import { randomUUID, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";
import {
	deleteCookie,
	getCookie,
	setCookie,
} from "@tanstack/react-start/server";
import { Pool } from "pg";

import { createDashboardService } from "#/lib/dashboard/core/service.server";
import {
	type CreateServiceFastResult,
	type DashboardConfig,
	DashboardConfigError,
	type DashboardDeploymentRecord,
	type DashboardDomainBinding,
	type DashboardGitHubAccount,
	type DashboardHomeState,
	type DashboardOnboardingDraft,
	type DashboardProject,
	type DashboardRepositoryInspection,
	type DashboardService,
	type DashboardServiceLogLine,
	type DashboardServiceLogType,
	type DashboardServicePosition,
	type DashboardServiceRecord,
	type DashboardServiceStatus,
	type DevLoginIdentity,
	type GitHubUserRepository,
	type UpdateServiceInput,
} from "#/lib/dashboard/core/types.server";
import {
	assertProductionDashboardConfig,
	formatDashboardStartupContract,
	parseRuntimeProfile,
	usesSecureCookies,
} from "#/lib/dashboard/core/profile.server";
import {
	parseDevUsers,
	parseIdentifier,
} from "#/lib/dashboard/core/utils.server";
import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";
import { createGitHubAppUserClient } from "#/lib/dashboard/github/auth.server";
import { createPostgresDashboardStore } from "#/lib/dashboard/store/postgres.server";
import type { GitHubTokenCipher } from "#/lib/dashboard/store/token-crypto.server";
import {
	createGitHubTokenCipher,
	decodeGitHubTokenEncryptionKey,
} from "#/lib/dashboard/store/token-crypto.server";
import {
	createPlatformGateway,
	ingestGitHubWebhook,
} from "#/lib/platform-grpc/gateway.server";
import type { IngestGitHubWebhookInput } from "#/lib/platform-grpc/types.server";

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
let dashboardService: DashboardService | undefined;

function getDashboardService(): DashboardService {
	const config = getConfig();
	dashboardService ??= createDashboardService(config, {
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
	return dashboardService;
}

function getConfig(): RuntimeConfig {
	config ??= readConfig();
	return config;
}

function getPool(): Pool {
	const current = getConfig();
	pool ??= new Pool({ connectionString: current.databaseURL });
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
		await createPostgresDashboardStore(getConfig(), getPool()).ensureInitialized();
	} catch {
		return { status: "not_ready", failed: ["migrations"] };
	}
	return { status: "ready" };
}

export function listDevLogins(): Array<DevLoginIdentity> {
	return getDashboardService().listDevLogins();
}

export function isGitHubLoginEnabled(): boolean {
	return getDashboardService().isGitHubLoginEnabled();
}

export function getPublicBaseURL(): string {
	return getConfig().publicBaseURL;
}

export function beginGitHubLogin(input: {
	redirectTo?: string;
}): Promise<string> {
	return getDashboardService().beginGitHubLogin(input);
}

export function completeAuthCallback(input: {
	code?: string;
	state?: string;
	userId?: string;
	email?: string;
	redirectTo?: string;
}): Promise<string> {
	return getDashboardService().completeAuthCallback(input);
}

export function loadDashboardHome(
	environmentId?: string,
): Promise<DashboardHomeState | null> {
	return getDashboardService().loadDashboardHome(environmentId);
}

export function createEnvironmentFromSession(input: {
	projectId: string;
	name: string;
}) {
	return getDashboardService().createEnvironmentFromSession(input);
}

export function duplicateEnvironmentFromSession(input: {
	sourceEnvironmentId: string;
	name: string;
	copyVariables: boolean;
}) {
	return getDashboardService().duplicateEnvironmentFromSession(input);
}

export function renameEnvironmentFromSession(input: {
	environmentId: string;
	name: string;
}) {
	return getDashboardService().renameEnvironmentFromSession(input);
}

export function deleteEnvironmentFromSession(environmentId: string) {
	return getDashboardService().deleteEnvironmentFromSession(environmentId);
}

export function deployEnvironmentFromSession(environmentId: string) {
	return getDashboardService().deployEnvironmentFromSession(environmentId);
}

export function loadGitHubCatalogFromSession(): Promise<{
	githubAccount?: DashboardGitHubAccount;
	repositories: Array<GitHubUserRepository>;
}> {
	return getDashboardService().loadGitHubCatalogFromSession();
}

export function inspectRepositorySourceFromSession(input: {
	repositorySelector: string;
}): Promise<DashboardRepositoryInspection | undefined> {
	return getDashboardService().inspectRepositorySourceFromSession(input);
}

export function createProjectFromSession(
	name: string,
): Promise<DashboardProject> {
	return getDashboardService().createProjectFromSession(name);
}

export function inspectRepositoryFromSession(input: {
	repositorySelector: string;
}): Promise<DashboardOnboardingDraft> {
	return getDashboardService().inspectRepositoryFromSession(input);
}

export function confirmRepositoryFromSession(input: {
	repositorySelector: string;
	serviceName?: string;
	trackedRef?: string;
	dockerfilePath?: string;
	contextDir?: string;
	cpuMillis?: number;
	memoryMebibytes?: number;
}): Promise<DashboardOnboardingDraft> {
	return getDashboardService().confirmRepositoryFromSession(input);
}

export function createServiceFastFromSession(input: {
	repositorySelector: string;
	serviceName?: string;
	trackedRef?: string;
	dockerfilePath?: string;
	contextDir?: string;
	cpuMillis?: number;
	memoryMebibytes?: number;
}): Promise<CreateServiceFastResult> {
	return getDashboardService().createServiceFastFromSession(input);
}

export function saveHostnameFromSession(
	hostname: string,
): Promise<DashboardOnboardingDraft> {
	return getDashboardService().saveHostnameFromSession(hostname);
}

export function publishDomainFromSession(): Promise<DashboardDomainBinding> {
	return getDashboardService().publishDomainFromSession();
}

export function getServiceStatusFromSession(input: {
	serviceId: string;
}): Promise<DashboardServiceStatus> {
	return getDashboardService().getServiceStatusFromSession(input);
}

export function listEnvironmentServicesFromSession(input: {
	environmentId: string;
}): Promise<Array<DashboardServiceRecord>> {
	return getDashboardService().listEnvironmentServicesFromSession(input);
}

export function waitForEnvironmentServicesFromSession(input: {
	environmentId: string;
	waitIndex: number;
	waitTimeoutSeconds: number;
}) {
	return getDashboardService().waitForEnvironmentServicesFromSession(input);
}

export function waitForServiceStatusFromSession(input: {
	serviceId: string;
	waitIndex: number;
	waitTimeoutSeconds: number;
}) {
	return getDashboardService().waitForServiceStatusFromSession(input);
}

export function waitForProjectServicesFromSession(input: {
	projectId: string;
	waitIndex: number;
	waitTimeoutSeconds: number;
}) {
	return getDashboardService().waitForProjectServicesFromSession(input);
}

export function listServiceLogsFromSession(input: {
	serviceId: string;
	allocationId?: string;
	limit?: number;
	logType?: DashboardServiceLogType;
	buildId?: string;
	search?: string;
	startTime?: Date;
	endTime?: Date;
}): Promise<Array<DashboardServiceLogLine>> {
	return getDashboardService().listServiceLogsFromSession(input);
}

export function listServiceDeploymentsFromSession(input: {
	serviceId: string;
	limit?: number;
}): Promise<Array<DashboardDeploymentRecord>> {
	return getDashboardService().listServiceDeploymentsFromSession(input);
}

export function updateServiceFromSession(
	input: UpdateServiceInput,
): Promise<DashboardServiceRecord> {
	return getDashboardService().updateServiceFromSession(input);
}

export function redeployServiceFromSession(input: {
	serviceId: string;
}): Promise<DashboardServiceStatus> {
	return getDashboardService().redeployServiceFromSession(input);
}

export function scaleServiceFromSession(input: {
	serviceId: string;
	desiredReplicaCount: number;
	confirmScaleToZero?: boolean;
}): Promise<DashboardServiceStatus> {
	return getDashboardService().scaleServiceFromSession(input);
}

export function restartServiceFromSession(input: {
	serviceId: string;
}): Promise<DashboardServiceStatus> {
	return getDashboardService().restartServiceFromSession(input);
}

export function discardServiceChangesFromSession(input: {
	serviceId: string;
	changeIds?: Array<string>;
	discardAll?: boolean;
}): Promise<DashboardServiceRecord> {
	return getDashboardService().discardServiceChangesFromSession(input);
}

export function deleteServiceFromSession(input: {
	serviceId: string;
}): Promise<void> {
	return getDashboardService().deleteServiceFromSession(input);
}

export function saveServicePositionFromSession(input: {
	environmentId: string;
	serviceId: string;
	position: DashboardServicePosition;
}): Promise<DashboardServicePosition> {
	return getDashboardService().saveServicePositionFromSession(input);
}

export function listDomainBindingsFromSession(input: {
	serviceId: string;
}): Promise<Array<DashboardDomainBinding>> {
	return getDashboardService().listDomainBindingsFromSession(input);
}

export function generateDomainBindingFromSession(input: {
	serviceId: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return getDashboardService().generateDomainBindingFromSession(input);
}

export function createDomainBindingFromSession(input: {
	serviceId: string;
	hostname: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return getDashboardService().createDomainBindingFromSession(input);
}

export function updateDomainBindingFromSession(input: {
	serviceId: string;
	hostname: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return getDashboardService().updateDomainBindingFromSession(input);
}

export function deleteDomainBindingFromSession(input: {
	hostname: string;
}): Promise<void> {
	return getDashboardService().deleteDomainBindingFromSession(input);
}

export function checkDomainDNSFromSession(
	hostname: string,
): Promise<DomainVerificationResult | undefined> {
	return getDashboardService().checkDomainDNSFromSession(hostname);
}

export function clearSession(): Promise<void> {
	return getDashboardService().clearSession();
}

export function refreshSession(): Promise<void> {
	return getDashboardService().refreshSession();
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
	if (Buffer.byteLength(userAssertionSecret, "utf8") < 32) {
		throw new DashboardConfigError({
			message:
				"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET must be at least 32 bytes",
		});
	}
	if (userAssertionSecret === jwtSecret) {
		throw new DashboardConfigError({
			message:
				"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET must be distinct from DASHBOARD_JWT_SECRET",
		});
	}
	if (
		githubTokenEncryptionKeyValue === jwtSecret ||
		githubTokenEncryptionKeyValue === userAssertionSecret ||
		keyMatchesSecret(githubTokenEncryptionKey, jwtSecret) ||
		keyMatchesSecret(githubTokenEncryptionKey, userAssertionSecret)
	) {
		throw new DashboardConfigError({
			message:
				"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY must be distinct from dashboard JWT and user-assertion secrets",
		});
	}
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
			userAssertionSecret,
			databaseURL,
			controlPlaneAddress,
			controlPlaneServerName,
		});
	}

	const loaded = {
		profile,
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
		controlPlaneAddress,
		controlPlaneServerName,
		jwtSecret,
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

function keyMatchesSecret(key: Buffer, secret: string): boolean {
	const candidate = Buffer.from(secret, "utf8");
	return candidate.length === key.length && timingSafeEqual(key, candidate);
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
