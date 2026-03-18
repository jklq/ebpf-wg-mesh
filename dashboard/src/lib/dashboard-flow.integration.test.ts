import { describe, expect, it } from "vitest";

import { createDashboardTestHarness } from "#/lib/dashboard.testkit";
import { completeLoginRoute } from "#/routes/auth/callback";
import { loadHomeRouteState } from "#/routes/index";
import { loadLoginRouteState } from "#/routes/login";
import { logoutRouteResponse } from "#/routes/logout";

describe("dashboard integration flow", () => {
	it("runs the authenticated session flow across route handlers and the dashboard service", async () => {
		const harness = createDashboardTestHarness();

		const loginState = await loadLoginRouteState(harness.service);
		expect(loginState.session).toBeNull();
		expect(loginState.devUsers).toEqual(harness.config.devUsers);

		const destination = await completeLoginRoute(harness.service, {
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/projects",
		});
		expect(destination).toBe("/projects");

		const initialHome = await loadHomeRouteState(harness.service);
		expect(initialHome).toMatchObject({
			user: {
				subject: "user-1",
				email: "user@example.com",
			},
			controlPlaneReachable: true,
			projects: [],
		});

		await harness.service.createProjectFromSession("demo-app");

		const updatedHome = await loadHomeRouteState(harness.service);
		expect(updatedHome.projects).toEqual([
			{
				id: "project-1",
				name: "demo-app",
				kind: "user",
			},
		]);

		const logout = await logoutRouteResponse(harness.service);
		expect(logout.status).toBe(302);
		expect(logout.headers.get("Location")).toBe("/login");
		await expect(harness.service.loadDashboardHome()).resolves.toBeNull();
	});

	it("keeps the app session through a degraded control-plane read and recovers cleanly", async () => {
		const harness = createDashboardTestHarness();

		await completeLoginRoute(harness.service, {
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		harness.platform.errors.ensurePrincipal = new Error("control plane down");

		const degradedHome = await loadHomeRouteState(harness.service);
		expect(degradedHome).toMatchObject({
			user: {
				subject: "user-1",
				email: "user@example.com",
			},
			controlPlaneReachable: false,
			controlPlaneError: "control plane down",
			projects: [],
		});

		delete harness.platform.errors.ensurePrincipal;

		await harness.service.createProjectFromSession("recovered-app");

		const recoveredHome = await loadHomeRouteState(harness.service);
		expect(recoveredHome.controlPlaneReachable).toBe(true);
		expect(recoveredHome.projects).toEqual([
			{
				id: "project-1",
				name: "recovered-app",
				kind: "user",
			},
		]);
	});
});
