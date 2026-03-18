// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { createDashboardTestHarness } from "#/lib/dashboard.testkit";
import { completeLoginRoute } from "#/routes/auth/callback";
import { HomePageView, loadHomeRouteState } from "#/routes/index";
import { LoginPageView, loadLoginRouteState } from "#/routes/login";
import { logoutRouteResponse } from "#/routes/logout";

describe("dashboard routes", () => {
	it("loads login state from the dashboard service", async () => {
		const harness = createDashboardTestHarness();

		const state = await loadLoginRouteState(harness.service);

		expect(state.session).toBeNull();
		expect(state.devUsers).toEqual(harness.config.devUsers);
	});

	it("renders login identities with redirect links", () => {
		render(
			<LoginPageView
				state={{
					session: null,
					devUsers: [{ subject: "user-1", email: "user@example.com" }],
				}}
				redirectTo="/projects"
			/>,
		);

		expect(screen.getByText("user@example.com")).toBeTruthy();
		expect(
			screen.getByRole("link", { name: /continue/i }).getAttribute("href"),
		).toBe(
			"/auth/callback?subject=user-1&email=user%40example.com&redirect=%2Fprojects",
		);
	});

	it("redirects home route loads when there is no session", async () => {
		const harness = createDashboardTestHarness();

		await expect(loadHomeRouteState(harness.service)).rejects.toBeTruthy();
	});

	it("renders degraded control-plane state on the home page", () => {
		render(
			<HomePageView
				state={{
					user: {
						id: "user-1",
						subject: "user-1",
						email: "user@example.com",
					},
					projects: [],
					controlPlaneReachable: false,
					controlPlaneError: "control plane unavailable",
				}}
				projectName=""
				isPending={false}
				onProjectNameChange={vi.fn()}
				onSubmit={vi.fn()}
			/>,
		);

		expect(
			screen.getByText("Internal PlatformService not reachable"),
		).toBeTruthy();
		expect(screen.getByText("control plane unavailable")).toBeTruthy();
	});

	it("completes auth callback through the dashboard service", async () => {
		const harness = createDashboardTestHarness();

		const destination = await completeLoginRoute(harness.service, {
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/projects",
		});

		expect(destination).toBe("/projects");
	});

	it("logs out and redirects to /login", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeDevLogin({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		const response = await logoutRouteResponse(harness.service);

		expect(response.status).toBe(302);
		expect(response.headers.get("Location")).toBe("/login");
		expect(harness.cookies.values.has(harness.config.sessionCookieName)).toBe(
			false,
		);
	});
});
