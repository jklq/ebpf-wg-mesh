import { describe, expect, it } from "vitest";

import { createDashboardTestHarness } from "#/lib/dashboard.testkit";
import {
	AuthConflictError,
	authStateCookieOptions,
	refreshCookieOptions,
	sessionCookieOptions,
} from "#/lib/dashboard-core.server";
import {
	verifyAccessToken,
	verifyRefreshToken,
} from "#/lib/dashboard-jwt.server";

describe("dashboard service", () => {
	it("creates JWT auth cookies and sanitizes redirects on dev login", async () => {
		const harness = createDashboardTestHarness();

		const destination = await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "https://evil.example.test",
		});

		expect(destination).toBe("/");
		const accessToken = harness.cookies.values.get(
			harness.config.sessionCookieName,
		);
		const refreshToken = harness.cookies.values.get(
			harness.config.refreshCookieName,
		);
		expect(accessToken).toBeTruthy();
		expect(refreshToken).toBeTruthy();
		expect(
			verifyAccessToken(
				accessToken ?? "",
				harness.config,
				new Date("2026-03-18T12:05:00Z"),
			).user,
		).toMatchObject({
			id: "user-1",
			subject: "user-1",
			email: "user@example.com",
		});
		expect(
			verifyRefreshToken(
				refreshToken ?? "",
				harness.config,
				new Date("2026-03-18T12:05:00Z"),
			).sessionId,
		).toBe("session-1");
		expect(harness.storeEnsureInitializedCalls).toHaveLength(1);
		expect(harness.platform.ensurePrincipalCalls).toHaveLength(1);
	});

	it("keeps cookies secure for public https deployments but not localteststack domains", () => {
		const secureConfig = {
			...createDashboardTestHarness().config,
			publicBaseURL: "https://dashboard.example.test",
		};
		const localStackConfig = {
			...createDashboardTestHarness().config,
			publicBaseURL: "https://woozy-unextreme-genny.ngrok-free.dev",
			localDomainSuffix: "localtest.me",
		};

		expect(sessionCookieOptions(secureConfig, new Date()).secure).toBe(true);
		expect(refreshCookieOptions(secureConfig, new Date()).secure).toBe(true);
		expect(authStateCookieOptions(secureConfig, new Date()).secure).toBe(true);
		expect(sessionCookieOptions(localStackConfig, new Date()).secure).toBe(
			false,
		);
		expect(refreshCookieOptions(localStackConfig, new Date()).secure).toBe(
			false,
		);
		expect(authStateCookieOptions(localStackConfig, new Date()).secure).toBe(
			false,
		);
	});

	it("returns degraded home state when the control plane is unavailable", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
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
		await harness.service.completeAuthCallback({
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
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		await harness.service.clearSession();

		expect(harness.cookies.values.has(harness.config.sessionCookieName)).toBe(
			false,
		);
		expect(harness.cookies.values.has(harness.config.refreshCookieName)).toBe(
			false,
		);
		await expect(harness.service.loadDashboardHome()).resolves.toBeNull();
	});

	it("rotates refresh tokens and issues a new access token on refresh", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		const initialAccessToken = harness.cookies.values.get(
			harness.config.sessionCookieName,
		);
		const initialRefreshToken = harness.cookies.values.get(
			harness.config.refreshCookieName,
		);

		await harness.service.refreshSession();

		const rotatedAccessToken = harness.cookies.values.get(
			harness.config.sessionCookieName,
		);
		const rotatedRefreshToken = harness.cookies.values.get(
			harness.config.refreshCookieName,
		);
		expect(rotatedAccessToken).toBeTruthy();
		expect(rotatedRefreshToken).toBeTruthy();
		expect(rotatedAccessToken).not.toBe(initialAccessToken);
		expect(rotatedRefreshToken).not.toBe(initialRefreshToken);
		expect(
			verifyRefreshToken(
				rotatedRefreshToken ?? "",
				harness.config,
				new Date("2026-03-18T12:05:00Z"),
			).sessionId,
		).toBe("session-2");
		await expect(harness.service.loadDashboardHome()).resolves.not.toBeNull();
	});

	it("starts GitHub App user login by storing auth state and returning an authorization url", async () => {
		const harness = createDashboardTestHarness();

		const url = await harness.service.beginGitHubLogin({
			redirectTo: "/projects",
		});

		expect(url).toContain("https://github.example.test/login/oauth/authorize");
		expect(harness.cookies.values.has(harness.config.authStateCookieName)).toBe(
			true,
		);
	});

	it("completes GitHub callback for first signup", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.beginGitHubLogin({ redirectTo: "/projects" });

		const destination = await harness.service.completeAuthCallback({
			code: "github-code",
			state: "session-1",
		});

		expect(destination).toBe("/");
		expect(harness.platform.ensurePrincipalCalls).toHaveLength(1);
	});

	it("stores GitHub token metadata and initializes onboarding state after callback", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.beginGitHubLogin({ redirectTo: "/projects" });

		await harness.service.completeAuthCallback({
			code: "github-code",
			state: "session-1",
		});

		const home = await harness.service.loadDashboardHome();

		expect(home).not.toBeNull();
		expect(home?.githubAccount).toMatchObject({
			login: "octocat",
			primaryEmail: "user@example.com",
			accessToken: "github-access-token",
			tokenType: "bearer",
			scope: "read:user,user:email,repo",
		});
		expect(home?.onboarding).toMatchObject({
			currentStep: "account",
			projectId: "",
			serviceId: "",
			repositorySelector: "",
			trackedRef: "",
			dockerfilePath: "",
			contextDir: "",
			containerPort: "",
			hostname: "",
		});
	});

	it("updates an existing repository service to the requested container port", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.projects = [
			{
				id: "project-1",
				name: "octocat/hello",
				kind: "user",
			},
		];
		harness.platform.services = [
			{
				id: "service-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					provider: "github",
					repositorySelector: "octocat/hello",
					trackedRef: "main",
					buildRecipe: {
						dockerfilePath: "Dockerfile",
						contextDir: ".",
					},
					containerPort: 8080,
				},
			},
		];

		const draft = await harness.service.confirmRepositoryFromSession({
			repositorySelector: "octocat/hello",
			containerPort: "3001",
		});

		expect(harness.platform.createServiceCalls).toHaveLength(0);
		expect(harness.platform.updateServiceCalls).toEqual([
			{
				user: {
					id: "user-1",
					subject: "user-1",
					email: "user@example.com",
				},
				projectId: "project-1",
				serviceId: "service-1",
				source: {
					provider: "github",
					repositorySelector: "octocat/hello",
					trackedRef: "main",
					buildRecipe: {
						dockerfilePath: "Dockerfile",
						contextDir: ".",
					},
					containerPort: 3001,
				},
			},
		]);
		expect(draft.containerPort).toBe("3001");
	});

	it("auto-links a verified email match when there is exactly one existing user", async () => {
		const harness = createDashboardTestHarness();
		await harness.store.upsertDevUser("user-1", "user@example.com");
		await harness.service.beginGitHubLogin({ redirectTo: "/" });

		await harness.service.completeAuthCallback({
			code: "github-code",
			state: "session-1",
		});

		expect(harness.platform.ensurePrincipalCalls[0]?.email).toBe(
			"user@example.com",
		);
	});

	it("rejects GitHub callback when there is no verified email", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.beginGitHubLogin({ redirectTo: "/" });
		harness.github.identityError = new AuthConflictError({
			code: "missing_verified_email",
			message: "missing_verified_email",
		});

		await expect(
			harness.service.completeAuthCallback({
				code: "github-code",
				state: "session-1",
			}),
		).rejects.toMatchObject({ code: "missing_verified_email" });
	});
});
