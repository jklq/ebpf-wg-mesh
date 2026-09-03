import { DashboardConfigError } from "#/lib/dashboard/core/types.server";

export type RuntimeProfile = "development" | "production";

const defaultApplicationSecrets = new Set([
	"test-user-assertion-secret-at-least-32-bytes",
	"dashboard-test-secret",
	"changeme",
]);

export function parseRuntimeProfile(raw?: string): RuntimeProfile {
	const value = (raw ?? "").trim().toLowerCase();
	if (value === "" || value === "production") {
		return "production";
	}
	if (value === "development") {
		return "development";
	}
	throw new DashboardConfigError({
		message: "DASHBOARD_PROFILE must be development or production",
	});
}

export function assertProductionDashboardConfig(input: {
	devUsers: ReadonlyArray<unknown>;
	publicBaseURL: string;
	localDomainSuffix?: string;
	localIngressBaseURL?: string;
	jwtSecret: string;
	userAssertionSecret: string;
	databaseURL: string;
	controlPlaneAddress: string;
	controlPlaneServerName: string;
}): void {
	if (input.devUsers.length > 0) {
		throw new DashboardConfigError({
			message: "DASHBOARD_DEV_USERS are not allowed in production",
		});
	}
	if (
		input.localDomainSuffix?.trim() ||
		!usesSecureCookies(input.publicBaseURL)
	) {
		throw new DashboardConfigError({
			message: "insecure cookies are not allowed in production",
		});
	}
	if (input.localIngressBaseURL?.trim()) {
		throw new DashboardConfigError({
			message: "local ingress URLs are not allowed in production",
		});
	}
	assertProductionTLSName(
		"DASHBOARD_CONTROLPLANE_SERVER_NAME",
		input.controlPlaneServerName,
	);
	assertProductionHTTPSURL("DASHBOARD_PUBLIC_BASE_URL", input.publicBaseURL);
	assertProductionDurableURL("DASHBOARD_DATABASE_URL", input.databaseURL);
	assertProductionDurableDial(
		"DASHBOARD_CONTROLPLANE_ADDRESS",
		input.controlPlaneAddress,
	);
	assertProductionSecret("DASHBOARD_JWT_SECRET", input.jwtSecret);
	assertProductionSecret(
		"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET",
		input.userAssertionSecret,
	);
}

export function usesSecureCookies(publicBaseURL: string): boolean {
	return publicBaseURL.toLowerCase().startsWith("https://");
}

export function formatDashboardStartupContract(input: {
	profile: RuntimeProfile;
	githubEnabled: boolean;
	secureCookies: boolean;
	databaseURL: string;
	controlPlaneAddress: string;
}): string {
	const features = ["console"];
	if (input.githubEnabled) {
		features.push("github");
	}
	if (input.secureCookies) {
		features.push("secure_cookies");
	}
	const dependencies = [
		`database=${dependencyClassForURL(input.databaseURL)}`,
		`control_plane=${dependencyClassForURL(input.controlPlaneAddress)}`,
	];
	return `startup contract component=console profile=${input.profile} features=${features.join(",")} dependencies=${dependencies.join(",")}`;
}

function assertProductionTLSName(field: string, name: string): void {
	const value = name.trim();
	if (value === "") {
		throw new DashboardConfigError({
			message: `${field} is required in production`,
		});
	}
	if (value.includes("*")) {
		throw new DashboardConfigError({
			message: `${field} must not use a wildcard identity in production`,
		});
	}
}

function assertProductionHTTPSURL(field: string, raw: string): void {
	let parsed: URL;
	try {
		parsed = new URL(raw);
	} catch {
		throw new DashboardConfigError({
			message: `${field} must be a complete public https URL in production`,
		});
	}
	if (parsed.protocol !== "https:") {
		throw new DashboardConfigError({
			message: `${field} must be a complete public https URL in production`,
		});
	}
	const host = parsed.hostname.trim();
	if (
		host === "" ||
		isLoopbackHost(host) ||
		!host.includes(".") ||
		host.toLowerCase().endsWith(".local")
	) {
		throw new DashboardConfigError({
			message: `${field} must be a complete public https URL in production`,
		});
	}
}

function assertProductionDurableURL(field: string, raw: string): void {
	assertProductionDurableHost(field, hostnameFromDialTarget(raw));
}

function assertProductionDurableDial(field: string, raw: string): void {
	assertProductionDurableHost(field, hostnameFromDialTarget(raw));
}

function assertProductionDurableHost(field: string, host: string): void {
	if (host.trim() === "") {
		throw new DashboardConfigError({
			message: `${field} is required in production`,
		});
	}
	if (isLoopbackHost(host)) {
		throw new DashboardConfigError({
			message: `${field} must not use a loopback host as a durable service in production`,
		});
	}
}

function assertProductionSecret(field: string, value: string): void {
	if (value.trim() === "" || defaultApplicationSecrets.has(value)) {
		throw new DashboardConfigError({
			message: `${field} must not use a default or generated secret in production`,
		});
	}
}

function hostnameFromDialTarget(raw: string): string {
	const value = raw.trim();
	if (value === "") {
		return "";
	}
	if (value.includes("://")) {
		try {
			return new URL(value).hostname;
		} catch {
			return value;
		}
	}
	if (value.startsWith("[") && value.includes("]:")) {
		return value.slice(1, value.indexOf("]:"));
	}
	const colon = value.lastIndexOf(":");
	if (colon > 0 && !value.includes("/")) {
		return value.slice(0, colon);
	}
	return value;
}

function isLoopbackHost(host: string): boolean {
	const value = host.trim().toLowerCase();
	if (value === "localhost" || value === "::1") {
		return true;
	}
	return value === "127.0.0.1" || value.startsWith("127.");
}

function dependencyClassForURL(raw: string): string {
	if (raw.trim() === "") {
		return "unset";
	}
	return isLoopbackHost(hostnameFromDialTarget(raw)) ? "loopback" : "durable";
}
