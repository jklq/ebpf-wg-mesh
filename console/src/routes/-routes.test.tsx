// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { createDashboardTestHarness } from "#/lib/dashboard.testkit";
import type { DashboardHomeState } from "#/lib/dashboard.server";
import { GitHubApiError } from "#/lib/dashboard-core.server";
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
		expect(state.githubLoginEnabled).toBe(true);
		expect(state.publicBaseURL).toBe(harness.config.publicBaseURL);
	});

	it("renders GitHub-first login with dev fallback links", () => {
		render(
			<LoginPageView
				state={{
					session: null,
					devUsers: [{ subject: "user-1", email: "user@example.com" }],
					githubLoginEnabled: true,
					publicBaseURL: "https://dashboard.example.test",
				}}
				redirectTo="/projects"
			/>,
		);

		expect(
			screen.getByRole("link", { name: /continue with github/i }),
		).toBeTruthy();
		expect(
			screen
				.getByRole("link", { name: /continue with github/i })
				.getAttribute("href"),
		).toBe("https://dashboard.example.test/auth/start?redirect=%2Fprojects");
		expect(screen.getByText("Development logins")).toBeTruthy();
		expect(
			screen.getByText("user@example.com").closest("a")?.getAttribute("href"),
		).toBe(
			"/auth/callback?subject=user-1&email=user%40example.com&redirect=%2Fprojects",
		);
	});

	it("redirects home route loads when there is no session", async () => {
		const harness = createDashboardTestHarness();

		await expect(loadHomeRouteState(harness.service)).rejects.toBeTruthy();
	});

	it("renders onboarding cards with install-required repository state", () => {
		render(
			<HomePageView
				state={homeState({
					onboarding: {
						currentStep: "repository",
						projectId: "",
						serviceId: "",
						repositorySelector: "private/secret",
						trackedRef: "",
						dockerfilePath: "",
						contextDir: "",
						containerPort: "",
						hostname: "",
					},
					repositoryInspection: {
						accessState: "installation_required",
						defaultBranch: "main",
						dockerfileCandidates: [],
					},
				})}
				repositorySelector="private/secret"
				trackedRef=""
				dockerfilePath=""
				contextDir=""
				containerPort=""
				hostname=""
				isPending={false}
				onRepositorySelectorChange={vi.fn()}
				onTrackedRefChange={vi.fn()}
				onDockerfilePathChange={vi.fn()}
				onContextDirChange={vi.fn()}
				onContainerPortChange={vi.fn()}
				onHostnameChange={vi.fn()}
				onInspectRepository={vi.fn()}
				onConfirmRepository={vi.fn()}
				onCheckDNS={vi.fn()}
				onPublishDomain={vi.fn()}
				onRefresh={vi.fn()}
			/>,
		);

		expect(
			screen.getByText(
				"GitHub sign-in succeeded, but the GitHub App is not installed for this repository yet. Open the install flow, grant the repository, then return here. The page will re-check automatically.",
			),
		).toBeTruthy();
		expect(
			screen.getByRole("link", { name: /install github app/i }),
		).toBeTruthy();
	});

	it("renders a configuration hint when install-required state lacks an install url", () => {
		render(
			<HomePageView
				state={homeState({
					githubInstallURL: "",
					onboarding: {
						currentStep: "repository",
						projectId: "",
						serviceId: "",
						repositorySelector: "private/secret",
						trackedRef: "",
						dockerfilePath: "",
						contextDir: "",
						containerPort: "",
						hostname: "",
					},
					repositoryInspection: {
						accessState: "installation_required",
						defaultBranch: "main",
						dockerfileCandidates: [],
					},
				})}
				repositorySelector="private/secret"
				trackedRef=""
				dockerfilePath=""
				contextDir=""
				containerPort=""
				hostname=""
				isPending={false}
				onRepositorySelectorChange={vi.fn()}
				onTrackedRefChange={vi.fn()}
				onDockerfilePathChange={vi.fn()}
				onContextDirChange={vi.fn()}
				onContainerPortChange={vi.fn()}
				onHostnameChange={vi.fn()}
				onInspectRepository={vi.fn()}
				onConfirmRepository={vi.fn()}
				onCheckDNS={vi.fn()}
				onPublishDomain={vi.fn()}
				onRefresh={vi.fn()}
			/>,
		);

		expect(
			screen.getByText(
				"GitHub sign-in succeeded, but this repository still needs a GitHub App installation or repository grant. Configure DASHBOARD_GITHUB_INSTALL_URL to show the install link here.",
			),
		).toBeTruthy();
	});

	it("renders the ready-for-domain build state", () => {
		render(
			<HomePageView
				state={homeState({
					serviceStatus: {
						service: {
							id: "service-1",
							projectId: "project-1",
							name: "hello",
							latestBuild: {
								buildId: "build-1",
								state: "succeeded",
								commitSha: "abc",
								imageDigest: "sha256:123",
								failureReason: "",
							},
						},
						allocation: {
							phase: "Healthy",
							message: "container healthy",
							endpointAddr: "10.0.0.10:8080",
							healthy: true,
						},
					},
				})}
				repositorySelector="octocat/hello"
				trackedRef="main"
				dockerfilePath="Dockerfile"
				contextDir="."
				containerPort="8080"
				hostname=""
				isPending={false}
				onRepositorySelectorChange={vi.fn()}
				onTrackedRefChange={vi.fn()}
				onDockerfilePathChange={vi.fn()}
				onContextDirChange={vi.fn()}
				onContainerPortChange={vi.fn()}
				onHostnameChange={vi.fn()}
				onInspectRepository={vi.fn()}
				onConfirmRepository={vi.fn()}
				onCheckDNS={vi.fn()}
				onPublishDomain={vi.fn()}
				onRefresh={vi.fn()}
			/>,
		);

		expect(screen.getByText("Healthy and ready for domain.")).toBeTruthy();
		expect(screen.getByText("Enter a hostname and check DNS.")).toBeTruthy();
	});

	it("renders the published URL from the dashboard public base URL", () => {
		render(
			<HomePageView
				state={homeState({
					publicBaseURL: "http://platform.localtest.me:8080",
					localIngressBaseURL: "http://platform.localtest.me:8080",
					localDomainSuffix: "localtest.me",
					onboarding: {
						currentStep: "domain",
						projectId: "project-1",
						serviceId: "service-1",
						repositorySelector: "octocat/hello",
						trackedRef: "main",
						dockerfilePath: "Dockerfile",
						contextDir: ".",
						containerPort: "80",
						hostname: "nginx.localtest.me",
					},
					domainBindings: [
						{
							hostname: "nginx.localtest.me",
							projectId: "project-1",
							serviceId: "service-1",
						},
					],
				})}
				repositorySelector="octocat/hello"
				trackedRef="main"
				dockerfilePath="Dockerfile"
				contextDir="."
				containerPort="80"
				hostname="nginx.localtest.me"
				isPending={false}
				onRepositorySelectorChange={vi.fn()}
				onTrackedRefChange={vi.fn()}
				onDockerfilePathChange={vi.fn()}
				onContextDirChange={vi.fn()}
				onContainerPortChange={vi.fn()}
				onHostnameChange={vi.fn()}
				onInspectRepository={vi.fn()}
				onConfirmRepository={vi.fn()}
				onCheckDNS={vi.fn()}
				onPublishDomain={vi.fn()}
				onRefresh={vi.fn()}
			/>,
		);

		const link = screen
			.getAllByRole("link", {
				name: "http://nginx.localtest.me:8080",
			})
			.at(-1);
		expect(link).toBeTruthy();
		expect(link?.getAttribute("href")).toBe("http://nginx.localtest.me:8080");
	});

	it("completes auth callback through the dashboard service", async () => {
		const harness = createDashboardTestHarness();

		const destination = await completeLoginRoute(harness.service, {
			code: "",
			state: "",
			subject: "user-1",
			email: "user@example.com",
			redirectTo: "/projects",
		});

		expect(destination).toBe("/projects");
	});

	it("logs out and redirects to /login", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.completeAuthCallback({
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
		expect(harness.cookies.values.has(harness.config.refreshCookieName)).toBe(
			false,
		);
	});

	it("renders GitHub callback permission guidance on the login page", () => {
		render(
			<LoginPageView
				state={{
					session: null,
					devUsers: [],
					githubLoginEnabled: true,
					publicBaseURL: "https://dashboard.example.test",
				}}
				error="github_api_error"
				errorDetail="GitHub denied access to the user's email addresses."
			/>,
		);

		expect(
			screen.getByText("GitHub denied access to the user's email addresses."),
		).toBeTruthy();
	});

	it("surfaces GitHub API callback failures as typed route errors", async () => {
		const harness = createDashboardTestHarness();
		await harness.service.beginGitHubLogin({ redirectTo: "/" });
		harness.github.identityError = new GitHubApiError({
			operation: "githubGET:/user/emails",
			message: "GitHub request failed: 403",
			cause: new Error("forbidden"),
			status: 403,
		});

		await expect(
			harness.service.completeAuthCallback({
				code: "github-code",
				state: "session-1",
			}),
		).rejects.toMatchObject({
			_tag: "GitHubApiError",
			status: 403,
			operation: "githubGET:/user/emails",
		});
	});
});

function homeState(
	overrides: Partial<DashboardHomeState> = {},
): DashboardHomeState {
	return {
		user: {
			id: "user-1",
			subject: "user-1",
			email: "user@example.com",
		},
		onboarding: {
			currentStep: "account",
			projectId: "",
			serviceId: "",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			containerPort: "8080",
			hostname: "",
		},
		repositories: [],
		githubInstallURL:
			"https://github.example.test/apps/platform/installations/new",
		publicBaseURL: "https://dashboard.example.test",
		localIngressBaseURL: undefined,
		ingressTargetHost: "platform.example.test",
		localDomainSuffix: undefined,
		domainBindings: [],
		controlPlaneReachable: true,
		...overrides,
	};
}
