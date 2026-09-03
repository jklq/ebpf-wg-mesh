import { describe, expect, it } from "vitest";

import { DashboardConfigError } from "#/lib/dashboard/core/types.server";
import {
	assertProductionDashboardConfig,
	formatDashboardStartupContract,
	parseRuntimeProfile,
} from "./profile.server";

const validProduction = {
	devUsers: [],
	publicBaseURL: "https://dashboard.example.test",
	jwtSecret: "production-dashboard-jwt-secret-at-least-32",
	userAssertionSecret: "production-user-assertion-secret-at-least-32",
	databaseURL:
		"postgresql://platform@cockroach.example.test:26257/defaultdb?sslmode=verify-full",
	controlPlaneAddress: "controlplane.example.test:9443",
	controlPlaneServerName: "controlplane.example.test",
};

describe("dashboard production profile", () => {
	it("treats a missing profile as production", () => {
		expect(parseRuntimeProfile()).toBe("production");
		expect(parseRuntimeProfile("")).toBe("production");
	});

	it("requires an explicit development profile", () => {
		expect(parseRuntimeProfile("development")).toBe("development");
		expect(() => parseRuntimeProfile("staging")).toThrow(DashboardConfigError);
	});

	it("accepts the valid minimal production configuration", () => {
		expect(() =>
			assertProductionDashboardConfig(validProduction),
		).not.toThrow();
		const contract = formatDashboardStartupContract({
			profile: "production",
			githubEnabled: true,
			secureCookies: true,
			databaseURL: validProduction.databaseURL,
			controlPlaneAddress: validProduction.controlPlaneAddress,
		});
		expect(contract).toContain("component=console");
		expect(contract).toContain("profile=production");
		expect(contract).toContain("features=console,github,secure_cookies");
		expect(contract).toContain("database=durable");
		expect(contract).not.toContain("postgresql://");
		expect(contract).not.toContain("secret");
	});

	it.each([
		[
			"development users",
			{ devUsers: [{ id: "dev", email: "dev@example.test" }] },
			"DASHBOARD_DEV_USERS",
		],
		[
			"insecure cookies",
			{ publicBaseURL: "http://dashboard.example.test" },
			"insecure cookies",
		],
		[
			"local insecure cookie domain",
			{ localDomainSuffix: "localtest.me" },
			"insecure cookies",
		],
		[
			"wildcard tls identity",
			{ controlPlaneServerName: "*.example.test" },
			"wildcard identity",
		],
		[
			"missing tls identity",
			{ controlPlaneServerName: " " },
			"required in production",
		],
		[
			"loopback database",
			{
				databaseURL:
					"postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
			},
			"loopback host",
		],
		[
			"loopback control plane",
			{ controlPlaneAddress: "127.0.0.1:9443" },
			"loopback host",
		],
		[
			"default application secret",
			{ jwtSecret: "dashboard-test-secret" },
			"default or generated secret",
		],
		[
			"incomplete public url",
			{ publicBaseURL: "https://localhost:3000" },
			"complete public https URL",
		],
		[
			"incomplete public certificates",
			{ publicBaseURL: "http://dashboard.example.test" },
			"insecure cookies",
		],
	] as const)("rejects %s", (_name, override, want) => {
		expect(() =>
			assertProductionDashboardConfig({ ...validProduction, ...override }),
		).toThrow(want);
	});
});
