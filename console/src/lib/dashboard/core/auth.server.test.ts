import { describe, expect, it } from "vitest";
import {
	authStateCookieOptions,
	beginGitHubLogin,
	clearSession,
	completeAuthCallback,
	refreshSession,
	sessionCookieOptions,
} from "#/lib/dashboard/core/auth.server";
import {
	verifyAccessToken,
	verifyRefreshToken,
} from "#/lib/dashboard/core/jwt.server";
import { loadDashboardHome } from "#/lib/dashboard/core/operations-home.server";
import { loadGitHubCatalogFromSession } from "#/lib/dashboard/core/operations-onboarding.server";
import {
	AuthConflictError,
	GitHubApiError,
	type GitHubAppUserToken,
} from "#/lib/dashboard/core/types.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";

describe("dashboard authentication", () => {
	it("creates JWT auth cookies and sanitizes redirects on dev login", async () => {
		const harness = createDashboardTestHarness();

		const destination = await completeAuthCallback(harness.runtime, {
			userId: "user-1",
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
		expect(authStateCookieOptions(secureConfig, new Date()).secure).toBe(true);
		expect(sessionCookieOptions(localStackConfig, new Date()).secure).toBe(
			false,
		);
		expect(authStateCookieOptions(localStackConfig, new Date()).secure).toBe(
			false,
		);
	});

	it("clears the current session", async () => {
		const harness = createDashboardTestHarness();
		await completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		await clearSession(harness.runtime);

		expect(harness.cookies.values.has(harness.config.sessionCookieName)).toBe(
			false,
		);
		expect(harness.cookies.values.has(harness.config.refreshCookieName)).toBe(
			false,
		);
		await expect(loadDashboardHome(harness.runtime)).resolves.toBeNull();
	});

	it("rotates refresh tokens and issues a new access token on refresh", async () => {
		const harness = createDashboardTestHarness();
		await completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		const initialAccessToken = harness.cookies.values.get(
			harness.config.sessionCookieName,
		);
		const initialRefreshToken = harness.cookies.values.get(
			harness.config.refreshCookieName,
		);

		await refreshSession(harness.runtime);

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
		await expect(loadDashboardHome(harness.runtime)).resolves.not.toBeNull();
	});

	it("starts GitHub App user login by storing auth state and returning an authorization url", async () => {
		const harness = createDashboardTestHarness();

		const url = await beginGitHubLogin(harness.runtime, {
			redirectTo: "/projects",
		});

		expect(url).toContain("https://github.example.test/login/oauth/authorize");
		expect(harness.cookies.values.has(harness.config.authStateCookieName)).toBe(
			true,
		);
	});

	it("completes GitHub callback for first signup", async () => {
		const harness = createDashboardTestHarness();
		await beginGitHubLogin(harness.runtime, { redirectTo: "/projects" });

		const destination = await completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});

		expect(destination).toBe("/");
	});

	it("assigns the configured operator GitHub login its stable operator user ID", async () => {
		const harness = createDashboardTestHarness({
			operatorGitHubLogin: "OctoCat",
		});
		await beginGitHubLogin(harness.runtime, { redirectTo: "/" });

		await completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});

		const home = await loadDashboardHome(harness.runtime);
		expect(home?.user.id).toBe("github:octocat");
	});

	it("stores GitHub token metadata and initializes onboarding state after callback", async () => {
		const harness = createDashboardTestHarness();
		await beginGitHubLogin(harness.runtime, { redirectTo: "/projects" });

		await completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});

		const home = await loadDashboardHome(harness.runtime);

		expect(home).not.toBeNull();
		expect(home?.githubAccount).toMatchObject({
			login: "octocat",
			primaryEmail: "user@example.com",
			tokenType: "bearer",
			scope: "read:user,user:email,repo",
		});
		expect(home?.githubAccount).not.toHaveProperty("accessToken");
		expect(home?.githubAccount).not.toHaveProperty("refreshToken");
		expect(JSON.stringify(home)).not.toContain("github-access-token");
		expect(JSON.stringify(home)).not.toContain("github-refresh-token");
		expect(home?.onboarding).toMatchObject({
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
		await beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		await completeAuthCallback(harness.runtime, {
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

		const home = await loadDashboardHome(harness.runtime);
		expect(home?.repositories).toEqual([]);
		expect(harness.github.listRepositoriesCalls).toEqual([]);

		const catalog = await loadGitHubCatalogFromSession(harness.runtime);

		expect(catalog.repositories).toEqual(harness.github.nextRepositories);
		expect(harness.github.refreshedTokens).toEqual(["github-refresh-token"]);
		expect(harness.github.listRepositoriesCalls).toEqual([
			"expired-github-token",
			"fresh-github-token",
		]);
		const account = await harness.store.getGitHubAccount("user-1");
		expect(account?.accessToken).toBe("fresh-github-token");
		expect(account?.refreshToken).toBe("next-github-refresh-token");
	});

	it("serializes simultaneous rotating GitHub refreshes across store users", async () => {
		const harness = createDashboardTestHarness();
		harness.github.nextToken = {
			accessToken: "expired-github-token",
			tokenType: "bearer",
			scope: "",
			refreshToken: "single-use-refresh-token",
		} as GitHubAppUserToken;
		await beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		await completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});

		harness.github.nextToken = {
			accessToken: "winning-access-token",
			tokenType: "bearer",
			scope: "repo",
			refreshToken: "rotated-refresh-token",
		} as GitHubAppUserToken;
		for (let index = 0; index < 2; index += 1) {
			harness.github.listRepositoriesErrors.push(
				new GitHubApiError({
					operation: "listRepositories",
					message: "GitHub request failed: 401",
					cause: new Response(null, { status: 401 }),
					status: 401,
				}),
			);
		}

		const catalogs = await Promise.all([
			loadGitHubCatalogFromSession(harness.runtime),
			loadGitHubCatalogFromSession(harness.runtime),
		]);

		expect(catalogs[0]?.repositories).toEqual(harness.github.nextRepositories);
		expect(catalogs[1]?.repositories).toEqual(harness.github.nextRepositories);
		expect(harness.github.refreshedTokens).toEqual([
			"single-use-refresh-token",
		]);
		const account = await harness.store.getGitHubAccount("user-1");
		expect(account?.accessToken).toBe("winning-access-token");
		expect(account?.refreshToken).toBe("rotated-refresh-token");
	});

	it("does not let a stale successful refresh overwrite the lease winner", async () => {
		const harness = createDashboardTestHarness();
		await beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		await completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});
		const initial = await harness.store.getGitHubAccount("user-1");
		if (!initial) throw new Error("missing GitHub account");
		const startedAt = new Date("2026-03-18T12:00:00Z");
		await expect(
			harness.store.tryAcquireGitHubTokenRefresh({
				userID: "user-1",
				expectedTokenVersion: initial.tokenVersion,
				leaseID: "stale-lease",
				now: startedAt,
				leaseExpiresAt: new Date(startedAt.getTime() + 60_000),
			}),
		).resolves.toBe(true);
		await expect(
			harness.store.tryAcquireGitHubTokenRefresh({
				userID: "user-1",
				expectedTokenVersion: initial.tokenVersion,
				leaseID: "winning-lease",
				now: new Date(startedAt.getTime() + 60_001),
				leaseExpiresAt: new Date(startedAt.getTime() + 120_000),
			}),
		).resolves.toBe(true);

		await expect(
			harness.store.completeGitHubTokenRefresh({
				userID: "user-1",
				providerSubject: initial.providerSubject,
				expectedTokenVersion: initial.tokenVersion,
				leaseID: "winning-lease",
				token: {
					accessToken: "winning-access",
					refreshToken: "winning-refresh",
					tokenType: "bearer",
					scope: "repo",
				},
				fallbackRefreshToken: initial.refreshToken ?? "",
			}),
		).resolves.toMatchObject({ accessToken: "winning-access" });
		await expect(
			harness.store.completeGitHubTokenRefresh({
				userID: "user-1",
				providerSubject: initial.providerSubject,
				expectedTokenVersion: initial.tokenVersion,
				leaseID: "stale-lease",
				token: {
					accessToken: "stale-access",
					refreshToken: "stale-refresh",
					tokenType: "bearer",
					scope: "repo",
				},
				fallbackRefreshToken: initial.refreshToken ?? "",
			}),
		).resolves.toBeNull();
		await expect(
			harness.store.getGitHubAccount("user-1"),
		).resolves.toMatchObject({
			accessToken: "winning-access",
			refreshToken: "winning-refresh",
		});
	});

	it("auto-links a verified email match when there is exactly one existing user", async () => {
		const harness = createDashboardTestHarness();
		await harness.store.upsertDevUser("user-1", "user@example.com");
		await beginGitHubLogin(harness.runtime, { redirectTo: "/" });

		await completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});

		const account = await harness.store.getGitHubAccount("user-1");
		expect(account?.primaryEmail).toBe("user@example.com");
	});

	it("rejects GitHub callback when there is no verified email", async () => {
		const harness = createDashboardTestHarness();
		await beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		harness.github.identityError = new AuthConflictError({
			code: "missing_verified_email",
			message: "missing_verified_email",
		});

		await expect(
			completeAuthCallback(harness.runtime, {
				code: "github-code",
				state: "session-1",
			}),
		).rejects.toMatchObject({ code: "missing_verified_email" });
	});
});
