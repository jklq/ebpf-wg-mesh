import { Buffer } from "node:buffer";
import { randomUUID } from "node:crypto";
import {
	deleteCookie,
	getCookie,
	setCookie,
} from "@tanstack/react-start/server";
import { Pool } from "pg";

import {
	createDashboardService,
	type DashboardConfig,
	parseDevUsers,
	parseIdentifier,
} from "#/lib/dashboard-core.server";
import { createPostgresDashboardStore } from "#/lib/dashboard-store.server";
import { createPlatformGateway } from "#/lib/platform-grpc.server";

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
	cookies: {
		get: (name) => getCookie(name) ?? undefined,
		set: (name, value, options) => setCookie(name, value, options),
		delete: (name, options) => deleteCookie(name, options),
	},
	randomUUID,
});

export const listDevLogins = service.listDevLogins;
export const completeDevLogin = service.completeDevLogin;
export const loadDashboardHome = service.loadDashboardHome;
export const createProjectFromSession = service.createProjectFromSession;
export const clearSession = service.clearSession;

function readConfig(): RuntimeConfig {
	const databaseURL = mustEnv("DASHBOARD_DATABASE_URL");
	const databaseSchema = parseIdentifier(
		process.env.DASHBOARD_DATABASE_SCHEMA ?? "dashboard",
	);
	const sessionCookieName =
		process.env.DASHBOARD_SESSION_COOKIE_NAME ?? "dashboard_session";
	const publicBaseURL =
		process.env.DASHBOARD_PUBLIC_BASE_URL ?? "http://localhost:3000";
	return {
		databaseURL,
		databaseSchema,
		sessionCookieName,
		publicBaseURL,
		controlPlaneAddress: mustEnv("DASHBOARD_CONTROLPLANE_ADDRESS"),
		controlPlaneServerName:
			process.env.DASHBOARD_CONTROLPLANE_SERVER_NAME ?? "controlplane",
		controlPlaneCA: decodeBase64Env("DASHBOARD_CONTROLPLANE_CA_PEM_B64"),
		controlPlaneCert: decodeBase64Env("DASHBOARD_CONTROLPLANE_CERT_PEM_B64"),
		controlPlaneKey: decodeBase64Env("DASHBOARD_CONTROLPLANE_KEY_PEM_B64"),
		devUsers: parseDevUsers(process.env.DASHBOARD_DEV_USERS ?? ""),
		sessionMaxAgeSeconds: 7 * 24 * 60 * 60,
	};
}

function decodeBase64Env(name: string): Buffer {
	return Buffer.from(mustEnv(name), "base64");
}

function mustEnv(name: string): string {
	const value = process.env[name]?.trim();
	if (!value) {
		throw new Error(`missing required environment variable ${name}`);
	}
	return value;
}

export type {
	DashboardConfig,
	DashboardHomeState,
	DashboardProject,
	DashboardProjectKind,
	DashboardService,
	DashboardStore,
	DashboardUser,
	DevLoginIdentity,
	PlatformGateway,
	SessionCookies,
} from "#/lib/dashboard-core.server";
