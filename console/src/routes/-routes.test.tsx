// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type {
	CreateServiceFastResult,
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";
import { GitHubApiError } from "#/lib/dashboard/core/types.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";
import { completeLoginRoute } from "#/routes/auth/callback";
import { loadHomeRouteState, NewServiceModal } from "#/routes/index";
import { LoginPageView, loadLoginRouteState } from "#/routes/login";
import { logoutRouteResponse } from "#/routes/logout";
import {
	readBoundedRequestBody,
	WebhookPayloadTooLargeError,
} from "#/routes/webhooks/github";

afterEach(() => cleanup());

describe("dashboard routes", () => {
	it("rejects oversized webhook bodies while streaming", async () => {
		const request = new Request(
			"https://dashboard.example.test/webhooks/github",
			{
				method: "POST",
				body: "1234",
			},
		);

		await expect(readBoundedRequestBody(request, 3)).rejects.toBeInstanceOf(
			WebhookPayloadTooLargeError,
		);
	});

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
		expect(screen.getAllByText(/dev logins/i).length).toBeGreaterThan(0);
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

	it("deploys a repository immediately when selected", async () => {
		const state = homeState({
			repositories: [
				{
					owner: "octocat",
					name: "hello",
					fullName: "octocat/hello",
					private: false,
					defaultBranch: "main",
				},
			],
		});
		let submitted: unknown;

		render(
			<NewServiceModal
				state={state}
				onClose={() => {}}
				onCreated={() => {}}
				confirmRepository={async ({ data }) => {
					submitted = data;
					return fastCreateResult(data.repositorySelector);
				}}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: /octocat\/hello/i }));

		await waitFor(() =>
			expect(submitted).toEqual({ repositorySelector: "octocat/hello" }),
		);
		expect(screen.queryByText(/creating service and queuing build/i)).toBeNull();
	});

	it("surfaces deployment errors back on the picker", async () => {
		const state = homeState({
			repositories: [
				{
					owner: "octocat",
					name: "no-docker",
					fullName: "octocat/no-docker",
					private: false,
					defaultBranch: "main",
				},
			],
		});

		render(
			<NewServiceModal
				state={state}
				onClose={() => {}}
				onCreated={() => {}}
				confirmRepository={async () => {
					throw new Error("Build failed: no Dockerfile found");
				}}
			/>,
		);

		fireEvent.click(
			screen.getByRole("button", { name: /octocat\/no-docker/i }),
		);

		expect(
			await screen.findByText("Build failed: no Dockerfile found"),
		).toBeTruthy();
	});

	it("does not show GitHub App configuration before a GitHub account exists", () => {
		render(
			<NewServiceModal
				state={homeState({
					githubLoginURL: "/auth/start",
					githubInstallURL:
						"https://github.example.test/apps/platform/installations/new",
					onboarding: {
						...homeState().onboarding,
						repositorySelector: "",
					},
				})}
				onClose={() => {}}
				onCreated={() => {}}
			/>,
		);

		expect(screen.getByPlaceholderText("Search repositories…")).toBeTruthy();
		expect(
			screen.queryByRole("link", { name: /configure github app/i }),
		).toBeNull();
	});

	it("keeps the GitHub app action on top of the repository picker", () => {
		const state = homeState({
			githubAccount: {
				providerSubject: "github-1",
				login: "octocat",
				primaryEmail: "octocat@example.test",
				accessToken: "token",
				tokenType: "bearer",
				scope: "repo",
			},
			repositories: [
				{
					owner: "octocat",
					name: "hello",
					fullName: "octocat/hello",
					private: false,
					defaultBranch: "main",
				},
			],
		});

		render(
			<NewServiceModal state={state} onClose={() => {}} onCreated={() => {}} />,
		);

		const configure = screen.getByRole("link", {
			name: /configure github app/i,
		});
		expect(configure.getAttribute("style")).toContain(
			"background: var(--surface-raised)",
		);
		expect(configure.getAttribute("style")).toContain("color: var(--text)");
		expect(configure.getAttribute("style")).toContain(
			"font-family: var(--font-mono)",
		);
		expect(configure.querySelector("svg")?.getAttribute("style")).toContain(
			"color: var(--text)",
		);

		fireEvent.keyDown(screen.getByPlaceholderText("Search repositories…"), {
			key: "ArrowDown",
		});

		expect(
			screen
				.getByRole("button", { name: /octocat\/hello/i })
				.getAttribute("style"),
		).toContain("background: var(--surface-raised)");
		expect(
			screen
				.getByRole("button", { name: /octocat\/hello/i })
				.querySelector("svg")
				?.getAttribute("style"),
		).toContain("color: var(--text)");
	});

	it("does not use a prefilled repository selector as picker search text", () => {
		const state = homeState({
			onboarding: {
				...homeState().onboarding,
				repositorySelector: "octocat/prefilled",
			},
			githubAccount: {
				providerSubject: "github-1",
				login: "octocat",
				primaryEmail: "octocat@example.test",
				accessToken: "token",
				tokenType: "bearer",
				scope: "repo",
			},
			repositories: [
				{
					owner: "octocat",
					name: "hello",
					fullName: "octocat/hello",
					private: false,
					defaultBranch: "main",
				},
			],
		});

		render(
			<NewServiceModal state={state} onClose={() => {}} onCreated={() => {}} />,
		);

		const search = screen.getByPlaceholderText(
			"Search repositories…",
		) as HTMLInputElement;
		expect(search.value).toBe("");
		expect(
			screen.getByRole("button", { name: /octocat\/hello/i }),
		).toBeTruthy();

		fireEvent.change(search, { target: { value: "missing" } });
		expect(
			screen.queryByRole("button", { name: /octocat\/hello/i }),
		).toBeNull();

		fireEvent.change(search, { target: { value: "" } });
		expect(
			screen.getByRole("button", { name: /octocat\/hello/i }),
		).toBeTruthy();
	});

	it("does not render manual repository entry when no repositories are loaded", () => {
		render(
			<NewServiceModal
				state={homeState({
					githubAccount: {
						providerSubject: "github-1",
						login: "octocat",
						primaryEmail: "octocat@example.test",
						accessToken: "token",
						tokenType: "bearer",
						scope: "repo",
					},
					onboarding: {
						...homeState().onboarding,
						repositorySelector: "octocat/prefilled",
					},
					repositories: [],
				})}
				onClose={() => {}}
				onCreated={() => {}}
			/>,
		);

		expect(
			(screen.getByPlaceholderText("Search repositories…") as HTMLInputElement)
				.value,
		).toBe("");
		expect(screen.queryByPlaceholderText("owner/repo")).toBeNull();
		expect(screen.queryByRole("button", { name: /check/i })).toBeNull();
		expect(screen.queryByText(/repositories/i)).toBeNull();
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
			hostname: "",
		},
		repositories: [],
		services: [],
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

function fastCreateResult(
	repositorySelector: string,
): CreateServiceFastResult {
	return {
		project: { id: "project-1", name: "test-project", kind: "user" },
		service: {
			id: "service-1",
			projectId: "project-1",
			name: "hello",
			spec: {
				source: {
					provider: "github",
					repositorySelector,
					trackedRef: "main",
				},
				runtime: { env: {}, ports: [] },
			},
		},
		serviceStatus: null,
		onboarding: {
			currentStep: "build",
			projectId: "project-1",
			serviceId: "service-1",
			repositorySelector,
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
	};
}
