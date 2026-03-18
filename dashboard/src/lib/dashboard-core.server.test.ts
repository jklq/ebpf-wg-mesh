import { describe, expect, it } from "vitest";

import { createDashboardTestHarness } from "#/lib/dashboard.testkit";

describe("dashboard service", () => {
	it("creates a session and sanitizes redirects on login", async () => {
		const harness = createDashboardTestHarness();

		const destination = await harness.service.completeDevLogin({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "https://evil.example.test",
		});

		expect(destination).toBe("/");
		expect(harness.cookies.values.get(harness.config.sessionCookieName)).toBe(
			"session-1",
		);
		expect(harness.platform.ensurePrincipalCalls).toHaveLength(1);
	});

	it("returns degraded home state when the control plane is unavailable", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeDevLogin({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.errors.ensurePrincipal = new Error("control plane down");

		const state = await harness.service.loadDashboardHome();

		expect(state).not.toBeNull();
		expect(state?.controlPlaneReachable).toBe(false);
		expect(state?.controlPlaneError).toContain("control plane down");
	});

	it("creates projects from the active session", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeDevLogin({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		const project = await harness.service.createProjectFromSession("demo-app");

		expect(project.name).toBe("demo-app");
		expect(harness.platform.createProjectCalls).toEqual([
			{
				user: {
					id: "user-1",
					subject: "user-1",
					email: "user@example.com",
				},
				name: "demo-app",
			},
		]);
	});

	it("clears the current session", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeDevLogin({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		await harness.service.clearSession();

		expect(harness.cookies.values.has(harness.config.sessionCookieName)).toBe(
			false,
		);
		await expect(harness.service.loadDashboardHome()).resolves.toBeNull();
	});
});
