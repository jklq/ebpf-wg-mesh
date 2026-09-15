import { Buffer } from "node:buffer";
import { randomUUID, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";
import {
	deleteCookie,
	getCookie,
	setCookie,
} from "@tanstack/react-start/server";
import { Pool } from "pg";
import * as auth from "#/lib/dashboard/core/auth.server";
import * as domains from "#/lib/dashboard/core/operations-domains.server";
import * as fleet from "#/lib/dashboard/core/operations-fleet.server";
import * as home from "#/lib/dashboard/core/operations-home.server";
import * as onboarding from "#/lib/dashboard/core/operations-onboarding.server";
import * as services from "#/lib/dashboard/core/operations-services.server";
import {
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
	type CreateServiceFastResult,
	type DashboardAgentEnrollment,
	type DashboardAgentLifecycleState,
	type DashboardConfig,
	DashboardConfigError,
	type DashboardDeploymentAction,
	type DashboardDeploymentRecord,
	type DashboardDomainBinding,
	type DashboardFleet,
	type DashboardFleetAgent,
	type DashboardGitHubAccount,
	type DashboardHomeState,
	type DashboardProject,
	type DashboardServiceLogLine,
	type DashboardServiceLogType,
	type DashboardServicePosition,
	type DashboardServiceRecord,
	type DashboardServiceStatus,
	type DevLoginIdentity,
	type FleetAgentInput,
	type GitHubUserRepository,
	type UpdateServiceInput,
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

function getDashboardRuntime(): DashboardRuntime {
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

export function loadFleetFromSession(): Promise<DashboardFleet> {
	return fleet.loadFleetFromSession(getDashboardRuntime());
}

export function createFleetAgentFromSession(
	input: FleetAgentInput,
): Promise<DashboardAgentEnrollment> {
	return fleet.createFleetAgentFromSession(getDashboardRuntime(), input);
}

export function updateFleetAgentFromSession(
	input: FleetAgentInput,
): Promise<DashboardFleetAgent> {
	return fleet.updateFleetAgentFromSession(getDashboardRuntime(), input);
}

export function setFleetAgentLifecycleFromSession(input: {
	agentId: string;
	lifecycleState: DashboardAgentLifecycleState;
}): Promise<DashboardFleetAgent> {
	return fleet.setFleetAgentLifecycleFromSession(getDashboardRuntime(), input);
}

export function isGitHubLoginEnabled(): boolean {
	return Boolean(getConfig().github);
}

export function getPublicBaseURL(): string {
	return getConfig().publicBaseURL;
}

export function beginGitHubLogin(input: {
	redirectTo?: string;
}): Promise<string> {
	return auth.beginGitHubLogin(getDashboardRuntime(), input);
}

export function completeAuthCallback(input: {
	code?: string;
	state?: string;
	userId?: string;
	email?: string;
	redirectTo?: string;
}): Promise<string> {
	return auth.completeAuthCallback(getDashboardRuntime(), input);
}

export function loadDashboardHome(
	environmentId?: string,
): Promise<DashboardHomeState | null> {
	return home.loadDashboardHome(getDashboardRuntime(), environmentId);
}

export function createEnvironmentFromSession(input: {
	projectId: string;
	name: string;
}) {
	return onboarding.createEnvironmentFromSession(getDashboardRuntime(), input);
}

export function duplicateEnvironmentFromSession(input: {
	sourceEnvironmentId: string;
	name: string;
	copyVariables: boolean;
}) {
	return onboarding.duplicateEnvironmentFromSession(
		getDashboardRuntime(),
		input,
	);
}

export function renameEnvironmentFromSession(input: {
	environmentId: string;
	name: string;
}) {
	return onboarding.renameEnvironmentFromSession(getDashboardRuntime(), input);
}

export function updateEnvironmentAutoDeployFromSession(input: {
	environmentId: string;
	autoDeploy: boolean;
}) {
	return onboarding.updateEnvironmentAutoDeployFromSession(
		getDashboardRuntime(),
		input,
	);
}

export function deleteEnvironmentFromSession(environmentId: string) {
	return onboarding.deleteEnvironmentFromSession(
		getDashboardRuntime(),
		environmentId,
	);
}

export function releaseEnvironmentFromSession(environmentId: string) {
	return onboarding.releaseEnvironmentFromSession(
		getDashboardRuntime(),
		environmentId,
	);
}

export function loadGitHubCatalogFromSession(): Promise<{
	githubAccount?: DashboardGitHubAccount;
	repositories: Array<GitHubUserRepository>;
}> {
	return onboarding.loadGitHubCatalogFromSession(getDashboardRuntime());
}

export function createProjectFromSession(
	name: string,
): Promise<DashboardProject> {
	return onboarding.createProjectFromSession(getDashboardRuntime(), name);
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
	return onboarding.createServiceFastFromSession(getDashboardRuntime(), input);
}

export function listEnvironmentServicesFromSession(input: {
	environmentId: string;
}) {
	return services.listEnvironmentServicesFromSession(
		getDashboardRuntime(),
		input,
	);
}

export function waitForEnvironmentServicesFromSession(input: {
	environmentId: string;
	waitIndex: number;
	waitTimeoutSeconds: number;
}) {
	return services.waitForEnvironmentServicesFromSession(
		getDashboardRuntime(),
		input,
	);
}

export function waitForServiceStatusFromSession(input: {
	serviceId: string;
	waitIndex: number;
	waitTimeoutSeconds: number;
}) {
	return services.waitForServiceStatusFromSession(getDashboardRuntime(), input);
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
	return services.listServiceLogsFromSession(getDashboardRuntime(), input);
}

export function listServiceDeploymentsFromSession(input: {
	serviceId: string;
	limit?: number;
}): Promise<Array<DashboardDeploymentRecord>> {
	return services.listServiceDeploymentsFromSession(
		getDashboardRuntime(),
		input,
	);
}

export function updateServiceFromSession(
	input: UpdateServiceInput,
): Promise<DashboardServiceRecord> {
	return services.updateServiceFromSession(getDashboardRuntime(), input);
}

export function applyDeploymentActionFromSession(input: {
	serviceId: string;
	deploymentId: string;
	action: DashboardDeploymentAction;
	idempotencyKey: string;
	allocationId?: string;
}): Promise<DashboardServiceStatus> {
	return services.applyDeploymentActionFromSession(
		getDashboardRuntime(),
		input,
	);
}

export function scaleServiceFromSession(input: {
	serviceId: string;
	desiredReplicaCount: number;
}): Promise<DashboardServiceStatus> {
	return services.scaleServiceFromSession(getDashboardRuntime(), input);
}

export function discardServiceChangesFromSession(input: {
	serviceId: string;
	changeIds?: Array<string>;
	discardAll?: boolean;
}): Promise<DashboardServiceRecord> {
	return services.discardServiceChangesFromSession(
		getDashboardRuntime(),
		input,
	);
}

export function deleteServiceFromSession(input: {
	serviceId: string;
}): Promise<void> {
	return services.deleteServiceFromSession(getDashboardRuntime(), input);
}

export function saveServicePositionFromSession(input: {
	environmentId: string;
	serviceId: string;
	position: DashboardServicePosition;
}): Promise<DashboardServicePosition> {
	return services.saveServicePositionFromSession(getDashboardRuntime(), input);
}

export function listDomainBindingsFromSession(input: {
	serviceId: string;
}): Promise<Array<DashboardDomainBinding>> {
	return domains.listDomainBindingsFromSession(getDashboardRuntime(), input);
}

export function generateDomainBindingFromSession(input: {
	serviceId: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return domains.generateDomainBindingFromSession(getDashboardRuntime(), input);
}

export function createDomainBindingFromSession(input: {
	serviceId: string;
	hostname: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return domains.createDomainBindingFromSession(getDashboardRuntime(), input);
}

export function updateDomainBindingFromSession(input: {
	serviceId: string;
	hostname: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return domains.updateDomainBindingFromSession(getDashboardRuntime(), input);
}

export function deleteDomainBindingFromSession(input: {
	hostname: string;
}): Promise<void> {
	return domains.deleteDomainBindingFromSession(getDashboardRuntime(), input);
}

export function clearSession(): Promise<void> {
	return auth.clearSession(getDashboardRuntime());
}

export function refreshSession(): Promise<void> {
	return auth.refreshSession(getDashboardRuntime());
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
