import { Buffer } from "node:buffer";
import { randomUUID } from "node:crypto";
import {
	deleteCookie,
	getCookie,
	setCookie,
} from "@tanstack/react-start/server";
import { Pool } from "pg";

import { createDashboardService } from "#/lib/dashboard/core/service.server";
import {
	type DashboardConfig,
	DashboardConfigError,
	type DashboardDomainBinding,
	type DashboardHomeState,
	type DashboardOnboardingDraft,
	type DashboardProject,
	type DashboardServiceRecord,
	type DashboardServiceStatus,
	type DevLoginIdentity,
	type UpdateServiceInput,
} from "#/lib/dashboard/core/types.server";
import {
	parseDevUsers,
	parseIdentifier,
} from "#/lib/dashboard/core/utils.server";
import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";
import { createGitHubAppUserClient } from "#/lib/dashboard/github/auth.server";
import { createPostgresDashboardStore } from "#/lib/dashboard/store/postgres.server";
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
}

const config = readConfig();
const pool = new Pool({
	connectionString: config.databaseURL,
});
const service = createDashboardService(config, {
	store: createPostgresDashboardStore(config, pool),
	platform: createPlatformGateway(config),
	github: config.github ? createGitHubAppUserClient(config.github) : undefined,
	cookies: {
		get: (name) => getCookie(name) ?? undefined,
		set: (name, value, options) => setCookie(name, value, options),
		delete: (name, options) => deleteCookie(name, options),
	},
	now: () => new Date(),
	randomUUID,
});

export function listDevLogins(): Array<DevLoginIdentity> {
	return service.listDevLogins();
}

export function isGitHubLoginEnabled(): boolean {
	return service.isGitHubLoginEnabled();
}

export function getPublicBaseURL(): string {
	return config.publicBaseURL;
}

export function beginGitHubLogin(input: {
	redirectTo?: string;
}): Promise<string> {
	return service.beginGitHubLogin(input);
}

export function completeAuthCallback(input: {
	code?: string;
	state?: string;
	subject?: string;
	email?: string;
	redirectTo?: string;
}): Promise<string> {
	return service.completeAuthCallback(input);
}

export function loadDashboardHome(): Promise<DashboardHomeState | null> {
	return service.loadDashboardHome();
}

export function createProjectFromSession(
	name: string,
): Promise<DashboardProject> {
	return service.createProjectFromSession(name);
}

export function inspectRepositoryFromSession(input: {
	repositorySelector: string;
}): Promise<DashboardOnboardingDraft> {
	return service.inspectRepositoryFromSession(input);
}

export function confirmRepositoryFromSession(input: {
	repositorySelector: string;
	serviceName?: string;
	trackedRef?: string;
	dockerfilePath?: string;
	contextDir?: string;
}): Promise<DashboardOnboardingDraft> {
	return service.confirmRepositoryFromSession(input);
}

export function saveHostnameFromSession(
	hostname: string,
): Promise<DashboardOnboardingDraft> {
	return service.saveHostnameFromSession(hostname);
}

export function publishDomainFromSession(): Promise<DashboardDomainBinding> {
	return service.publishDomainFromSession();
}

export function getServiceStatusFromSession(input: {
	projectId: string;
	serviceId: string;
}): Promise<DashboardServiceStatus> {
	return service.getServiceStatusFromSession(input);
}

export function updateServiceFromSession(
	input: UpdateServiceInput,
): Promise<DashboardServiceRecord> {
	return service.updateServiceFromSession(input);
}

export function listDomainBindingsFromSession(input: {
	projectId: string;
	serviceId: string;
}): Promise<Array<DashboardDomainBinding>> {
	return service.listDomainBindingsFromSession(input);
}

export function createDomainBindingFromSession(input: {
	projectId: string;
	serviceId: string;
	hostname: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return service.createDomainBindingFromSession(input);
}

export function updateDomainBindingFromSession(input: {
	projectId: string;
	serviceId: string;
	hostname: string;
	targetPort: string | number | undefined;
}): Promise<DashboardDomainBinding> {
	return service.updateDomainBindingFromSession(input);
}

export function deleteDomainBindingFromSession(input: {
	projectId: string;
	hostname: string;
}): Promise<void> {
	return service.deleteDomainBindingFromSession(input);
}

export function checkDomainDNSFromSession(
	hostname: string,
): Promise<DomainVerificationResult | undefined> {
	return service.checkDomainDNSFromSession(hostname);
}

export function clearSession(): Promise<void> {
	return service.clearSession();
}

export function refreshSession(): Promise<void> {
	return service.refreshSession();
}

export function forwardGitHubWebhook(
	input: IngestGitHubWebhookInput,
): Promise<void> {
	return ingestGitHubWebhook(config, input);
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

function mustEnv(name: string): string {
	const value = process.env[name]?.trim();
	if (!value) {
		throw new DashboardConfigError({
			message: `missing required environment variable ${name}`,
		});
	}
	return value;
}
