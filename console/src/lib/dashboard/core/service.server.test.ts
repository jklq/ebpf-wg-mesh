import { describe, expect, it } from "vitest";
import {
	authStateCookieOptions,
	refreshCookieOptions,
	sessionCookieOptions,
} from "#/lib/dashboard/core/auth.server";
import {
	createSessionTokenPair,
	verifyAccessToken,
	verifyRefreshToken,
} from "#/lib/dashboard/core/jwt.server";
import {
	AuthConflictError,
	GitHubApiError,
	type GitHubAppUserToken,
} from "#/lib/dashboard/core/types.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";

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
			publicBaseURL: "https://mesh.dev.example.test",
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
			hostname: "",
		});
	});

	it("refreshes an expired GitHub token before listing repositories", async () => {
		const harness = createDashboardTestHarness();
		harness.github.nextToken = {
			accessToken: "expired-github-token",
			tokenType: "bearer",
			scope: "",
			refreshToken: "github-refresh-token",
		} as GitHubAppUserToken;
		await harness.service.beginGitHubLogin({ redirectTo: "/" });
		await harness.service.completeAuthCallback({
			code: "github-code",
			state: "session-1",
		});

		harness.github.nextToken = {
			accessToken: "fresh-github-token",
			tokenType: "bearer",
			scope: "",
			refreshToken: "next-github-refresh-token",
		} as GitHubAppUserToken;
		harness.github.listRepositoriesErrors.push(
			new GitHubApiError({
				operation: "listRepositories",
				message: "GitHub request failed: 401",
				cause: new Response(null, { status: 401 }),
				status: 401,
			}),
		);

		const home = await harness.service.loadDashboardHome();

		expect(home?.repositories).toEqual(harness.github.nextRepositories);
		expect(harness.github.refreshedTokens).toEqual(["github-refresh-token"]);
		expect(harness.github.listRepositoriesCalls).toEqual([
			"expired-github-token",
			"fresh-github-token",
		]);
		const account = await harness.store.getGitHubAccount("user-1");
		expect(account?.accessToken).toBe("fresh-github-token");
		expect(account?.refreshToken).toBe("next-github-refresh-token");
	});

	it("initializes the dashboard schema before reading a home page from an existing access token", async () => {
		const harness = createDashboardTestHarness();
		const session = createSessionTokenPair(
			harness.config,
			{
				id: "user-1",
				subject: "user-1",
				email: "user@example.com",
			},
			"session-1",
			new Date("2026-03-18T12:00:00Z"),
		);
		harness.cookies.values.set(
			harness.config.sessionCookieName,
			session.accessToken,
		);

		const home = await harness.service.loadDashboardHome();

		expect(home).not.toBeNull();
		expect(harness.storeEnsureInitializedCalls).toHaveLength(1);
		expect(harness.storeEnsureSessionUserCalls).toHaveLength(1);
	});

	it("creates another service when deploying the same repository again", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.projects = [
			{
				id: "project-1",
				name: "brisk-harbor",
				kind: "user",
			},
		];
		harness.platform.services = [
			{
				id: "service-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					source: {
						provider: "github",
						repositorySelector: "octocat/hello",
						trackedRef: "main",
						buildRecipe: {
							dockerfilePath: "Dockerfile",
							contextDir: ".",
						},
					},
					runtime: { ports: [{ port: 8080, primary: true }] },
				},
			},
		];
		// Pre-link the project in the onboarding draft so it is reused by ID
		await harness.store.saveOnboardingDraft("user-1", {
			currentStep: "build",
			projectId: "project-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		});

		const draft = await harness.service.confirmRepositoryFromSession({
			repositorySelector: "octocat/hello",
			serviceName: "talented-harmony",
		});

		expect(harness.platform.updateServiceCalls).toHaveLength(0);
		expect(harness.platform.createServiceCalls).toEqual([
			{
				user: {
					id: "user-1",
					subject: "user-1",
					email: "user@example.com",
				},
				projectId: "project-1",
				name: "talented-harmony",
				spec: {
					source: {
						provider: "github",
						repositorySelector: "octocat/hello",
						trackedRef: "main",
						buildRecipe: {
							dockerfilePath: "Dockerfile",
							contextDir: ".",
						},
					},
					runtime: { ports: [] },
				},
			},
		]);
		expect(draft.serviceId).toBe("service-2");
	});

	it("seeds service runtime ports from repository inspection", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.nextRepositoryInspection = {
			accessState: "available",
			defaultBranch: "main",
			dockerfileCandidates: ["Dockerfile"],
			recommendedBuildRecipe: {
				dockerfilePath: "Dockerfile",
				contextDir: ".",
			},
			recommendedPorts: [3000, 8080],
		};

		await harness.service.confirmRepositoryFromSession({
			repositorySelector: "octocat/hello",
			serviceName: "talented-harmony",
		});

		expect(harness.platform.createServiceCalls[0].spec.runtime.ports).toEqual([
			{ port: 3000, primary: true },
			{ port: 8080, primary: false },
		]);
	});

	it("returns fast-created service details for immediate rendering", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		const result = await harness.service.createServiceFastFromSession({
			repositorySelector: "octocat/hello",
			serviceName: "talented-harmony",
		});

		expect(result.project.id).toBe("project-1");
		expect(result.service).toMatchObject({
			id: "service-1",
			projectId: "project-1",
			name: "talented-harmony",
		});
		expect(result.serviceStatus?.service.id).toBe("service-1");
		expect(result.onboarding).toMatchObject({
			currentStep: "build",
			projectId: "project-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
		});
	});

	it("forwards rich service log filters to the platform", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.serviceLogs = [
			{
				allocationId: "",
				agentId: "",
				stream: "deploy",
				rolloutGeneration: 1,
				sequence: 1,
				line: "initializing service",
				logType: "deploy",
				buildId: "build-1",
				stage: "initialization",
			},
		];

		const logs = await harness.service.listServiceLogsFromSession({
			projectId: "project-1",
			serviceId: "service-1",
			limit: 500,
			logType: "deploy",
			buildId: "build-1",
			search: "initializing",
		});

		expect(logs).toHaveLength(1);
		expect(harness.platform.listServiceLogsCalls[0]).toMatchObject({
			projectId: "project-1",
			serviceId: "service-1",
			limit: 500,
			logType: "deploy",
			buildId: "build-1",
			search: "initializing",
		});
	});

	it("updates a service display name with settings changes", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.services = [
			{
				id: "service-1",
				projectId: "project-1",
				name: "old-name",
				spec: {
					source: {
						provider: "github",
						repositorySelector: "octocat/hello",
						trackedRef: "main",
						buildRecipe: {
							dockerfilePath: "Dockerfile",
							contextDir: ".",
						},
					},
					runtime: { ports: [{ port: 8080, primary: true }] },
				},
			},
		];

		const updated = await harness.service.updateServiceFromSession({
			projectId: "project-1",
			serviceId: "service-1",
			serviceName: "talented-harmony",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
		});

		expect(updated.name).toBe("talented-harmony");
		expect(harness.platform.updateServiceCalls[0]).toMatchObject({
			projectId: "project-1",
			serviceId: "service-1",
			name: "talented-harmony",
		});
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
