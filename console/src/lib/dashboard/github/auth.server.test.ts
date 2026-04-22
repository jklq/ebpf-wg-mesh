import { afterEach, describe, expect, it, vi } from "vitest";

import { createGitHubAppUserClient } from "#/lib/dashboard/github/auth.server";

const originalFetch = globalThis.fetch;

afterEach(() => {
	globalThis.fetch = originalFetch;
	vi.restoreAllMocks();
});

describe("GitHub App user client", () => {
	it("lists repositories granted to GitHub App installations", async () => {
		const requests: Array<URL> = [];
		globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
			const url = new URL(input instanceof Request ? input.url : String(input));
			requests.push(url);
			const payload = url.pathname.endsWith("/repositories")
				? {
						total_count: 1,
						repositories: [
							{
								name: "hello",
								full_name: "octo-org/hello",
								private: false,
								default_branch: "main",
								owner: { login: "octo-org" },
							},
						],
					}
				: {
						total_count: 1,
						installations: [{ id: 42 }],
					};
			return new Response(JSON.stringify(payload), {
				status: 200,
				headers: { "content-type": "application/json" },
			});
		}) as unknown as typeof fetch;

		const client = createGitHubAppUserClient({
			appId: "app-1",
			clientId: "client-1",
			clientSecret: "secret-1",
			authorizationBaseURL: "https://github.example.test",
			apiBaseURL: "https://api.github.example.test",
		});

		const repositories = await client.listRepositories("token-1");

		expect(requests.map((request) => request.pathname)).toEqual([
			"/user/installations",
			"/user/installations/42/repositories",
		]);
		expect(repositories).toEqual([
			{
				owner: "octo-org",
				name: "hello",
				fullName: "octo-org/hello",
				private: false,
				defaultBranch: "main",
			},
		]);
	});

	it("paginates GitHub App installation repositories", async () => {
		const requests: Array<URL> = [];
		globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
			const url = new URL(input instanceof Request ? input.url : String(input));
			requests.push(url);
			if (url.pathname === "/user/installations") {
				return new Response(
					JSON.stringify({ total_count: 1, installations: [{ id: 42 }] }),
					{
						status: 200,
						headers: { "content-type": "application/json" },
					},
				);
			}
			const page = url.searchParams.get("page");
			const repositories =
				page === "1"
					? Array.from({ length: 100 }, (_, index) => ({
							name: `repo-${index}`,
							full_name: `octo-org/repo-${index}`,
							private: false,
							default_branch: "main",
							owner: { login: "octo-org" },
						}))
					: [
							{
								name: "repo-100",
								full_name: "octo-org/repo-100",
								private: false,
								default_branch: "main",
								owner: { login: "octo-org" },
							},
						];
			return new Response(
				JSON.stringify({
					total_count: 101,
					repositories,
				}),
				{
					status: 200,
					headers: { "content-type": "application/json" },
				},
			);
		}) as unknown as typeof fetch;

		const client = createGitHubAppUserClient({
			appId: "app-1",
			clientId: "client-1",
			clientSecret: "secret-1",
			authorizationBaseURL: "https://github.example.test",
			apiBaseURL: "https://api.github.example.test",
		});

		const repositories = await client.listRepositories("token-1");

		expect(
			requests.filter((request) => request.pathname.endsWith("/repositories")),
		).toHaveLength(2);
		expect(repositories).toHaveLength(101);
	});
});
