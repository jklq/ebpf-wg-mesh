import { Effect, Schema } from "effect";

import {
	AuthConflictError,
	GitHubApiError,
	type GitHubAppUserAuthConfig,
	type GitHubAppUserClient,
	type GitHubAppUserIdentity,
	type GitHubAppUserToken,
	type GitHubUserRepository,
} from "#/lib/dashboard-core.server";

const GitHubTokenResponseSchema = Schema.Struct({
	access_token: Schema.NonEmptyString,
	token_type: Schema.NonEmptyString,
	scope: Schema.optionalKey(Schema.String),
	expires_in: Schema.optionalKey(Schema.Number),
	refresh_token: Schema.optionalKey(Schema.String),
	refresh_token_expires_in: Schema.optionalKey(Schema.Number),
});

const GitHubUserResponseSchema = Schema.Struct({
	id: Schema.Union([Schema.String, Schema.Number]),
	login: Schema.NonEmptyString,
});

const GitHubEmailResponseSchema = Schema.Array(
	Schema.Struct({
		email: Schema.NonEmptyString,
		primary: Schema.Boolean,
		verified: Schema.Boolean,
	}),
);

const GitHubRepositoryResponseSchema = Schema.Array(
	Schema.Struct({
		name: Schema.NonEmptyString,
		full_name: Schema.NonEmptyString,
		private: Schema.Boolean,
		default_branch: Schema.optionalKey(Schema.String),
		owner: Schema.Struct({
			login: Schema.NonEmptyString,
		}),
	}),
);

export function createGitHubAppUserClient(
	config: GitHubAppUserAuthConfig,
): GitHubAppUserClient {
	return {
		buildAuthorizationURL(input) {
			const url = new URL(
				"/login/oauth/authorize",
				config.authorizationBaseURL,
			);
			url.searchParams.set("client_id", config.clientId);
			url.searchParams.set("redirect_uri", input.redirectURI);
			url.searchParams.set("state", input.state);
			url.searchParams.set("scope", "read:user user:email repo");
			return url.toString();
		},

		exchangeCode(input) {
			return Effect.runPromise(exchangeCodeEffect(config, input));
		},

		fetchIdentity(accessToken) {
			return Effect.runPromise(fetchIdentityEffect(config, accessToken));
		},

		listRepositories(accessToken) {
			return Effect.runPromise(listRepositoriesEffect(config, accessToken));
		},
	};
}

const exchangeCodeEffect = (
	config: GitHubAppUserAuthConfig,
	input: {
		code: string;
		redirectURI: string;
	},
): Effect.Effect<GitHubAppUserToken, GitHubApiError> =>
	Effect.gen(function* () {
		const tokenURL = new URL(
			"/login/oauth/access_token",
			config.authorizationBaseURL,
		);
		tokenURL.searchParams.set("client_id", config.clientId);
		tokenURL.searchParams.set("client_secret", config.clientSecret);
		tokenURL.searchParams.set("code", input.code);
		tokenURL.searchParams.set("redirect_uri", input.redirectURI);

			const response = yield* fetchEffect("exchangeCode", tokenURL, {
			method: "POST",
			headers: {
				Accept: "application/json",
				"User-Agent": `ebpf-wg-mesh-github-app/${config.appId}`,
			},
		});
			const payload = yield* readJsonEffect("exchangeCode.response", response).pipe(
				Effect.flatMap((payload) =>
					decodeGitHubTokenResponseEffect(payload, response.status),
				),
			);
			return {
			accessToken: payload.access_token,
			tokenType: payload.token_type,
			scope: payload.scope ?? "",
			accessTokenExpiresAt: readOptionalDate(payload.expires_in),
			refreshToken: payload.refresh_token,
			refreshTokenExpiresAt: readOptionalDate(
				payload.refresh_token_expires_in,
			),
		};
	}).pipe(Effect.withSpan("github.exchangeCode"));

const fetchIdentityEffect = (
	config: GitHubAppUserAuthConfig,
	accessToken: string,
): Effect.Effect<GitHubAppUserIdentity, AuthConflictError | GitHubApiError> =>
	Effect.gen(function* () {
			const userResponse = yield* githubGETEffect(config, "/user", accessToken);
			const user = yield* readJsonEffect("fetchIdentity.user", userResponse).pipe(
				Effect.flatMap((payload) =>
					decodeGitHubUserResponseEffect(payload, userResponse.status),
				),
			);
		const emailsResponse = yield* githubGETEffect(
			config,
			"/user/emails",
			accessToken,
		);
			const emails = yield* readJsonEffect(
				"fetchIdentity.emails",
				emailsResponse,
			).pipe(
				Effect.flatMap((payload) =>
					decodeGitHubEmailResponseEffect(payload, emailsResponse.status),
				),
			);
		const primaryVerified = emails.find(
			(entry) => entry.primary === true && entry.verified === true,
		);
		if (!primaryVerified) {
			return yield* Effect.fail(
				new AuthConflictError({
					code: "missing_verified_email",
					message:
						"GitHub account does not expose a verified primary email",
				}),
			);
		}
		return {
			providerSubject: String(user.id),
			login: user.login,
			primaryEmail: primaryVerified.email,
		};
	}).pipe(Effect.withSpan("github.fetchIdentity"));

const listRepositoriesEffect = (
	config: GitHubAppUserAuthConfig,
	accessToken: string,
): Effect.Effect<Array<GitHubUserRepository>, GitHubApiError> =>
	Effect.gen(function* () {
		const repositories: Array<GitHubUserRepository> = [];
		for (let page = 1; page <= 5; page += 1) {
			const url = new URL("/user/repos", config.apiBaseURL);
			url.searchParams.set("affiliation", "owner");
			url.searchParams.set("sort", "updated");
			url.searchParams.set("per_page", "100");
			url.searchParams.set("page", String(page));
			const response = yield* fetchEffect(`listRepositories:${page}`, url, {
				headers: {
					Accept: "application/vnd.github+json",
					Authorization: `Bearer ${accessToken}`,
					"User-Agent": `ebpf-wg-mesh-github-app/${config.appId}`,
					"X-GitHub-Api-Version": "2022-11-28",
				},
			});
			const payload = yield* readJsonEffect(
				`listRepositories.${page}`,
				response,
			).pipe(
				Effect.flatMap((value) =>
					Schema.decodeUnknownEffect(GitHubRepositoryResponseSchema)(value).pipe(
						Effect.mapError(
							(cause) =>
								new GitHubApiError({
									operation: `listRepositories.${page}`,
									message: `GitHub response validation failed: ${formatCause(cause)}`,
									cause,
									status: response.status,
								}),
						),
					),
				),
			);
			for (const repository of payload) {
				repositories.push({
					owner: repository.owner.login,
					name: repository.name,
					fullName: repository.full_name,
					private: repository.private,
					defaultBranch: repository.default_branch ?? "",
				});
			}
			if (payload.length < 100) {
				break;
			}
		}
		return repositories;
	}).pipe(Effect.withSpan("github.listRepositories"));

function githubGETEffect(
	config: GitHubAppUserAuthConfig,
	path: string,
	accessToken: string,
): Effect.Effect<Response, GitHubApiError> {
	const url = new URL(path, config.apiBaseURL);
	return fetchEffect(`githubGET:${path}`, url, {
		headers: {
			Accept: "application/vnd.github+json",
			Authorization: `Bearer ${accessToken}`,
			"User-Agent": `ebpf-wg-mesh-github-app/${config.appId}`,
			"X-GitHub-Api-Version": "2022-11-28",
		},
	});
}

function fetchEffect(
	operation: string,
	url: URL,
	init: RequestInit,
): Effect.Effect<Response, GitHubApiError> {
	return Effect.tryPromise({
		try: (signal) => fetch(url, { ...init, signal }),
		catch: (cause) =>
			new GitHubApiError({
				operation,
				message: formatCause(cause),
				cause,
			}),
	}).pipe(
		Effect.flatMap((response) =>
			response.ok
				? Effect.succeed(response)
				: Effect.fail(
						new GitHubApiError({
							operation,
							message: `GitHub request failed: ${response.status}`,
							cause: response,
							status: response.status,
						}),
					),
		),
	);
}

function readJsonEffect(
	operation: string,
	response: Response,
): Effect.Effect<unknown, GitHubApiError> {
	return Effect.tryPromise({
		try: () => response.json(),
		catch: (cause) =>
			new GitHubApiError({
				operation,
				message: `GitHub JSON decode failed: ${formatCause(cause)}`,
				cause,
				status: response.status,
			}),
		});
}

function decodeGitHubTokenResponseEffect(
	payload: unknown,
	status: number,
) {
	return Schema.decodeUnknownEffect(GitHubTokenResponseSchema)(payload).pipe(
		Effect.mapError(
			(cause) =>
				new GitHubApiError({
					operation: "exchangeCode.response",
					message: `GitHub response validation failed: ${formatCause(cause)}`,
					cause,
					status,
				}),
		),
	);
}

function decodeGitHubUserResponseEffect(payload: unknown, status: number) {
	return Schema.decodeUnknownEffect(GitHubUserResponseSchema)(payload).pipe(
		Effect.mapError(
			(cause) =>
				new GitHubApiError({
					operation: "fetchIdentity.user",
					message: `GitHub response validation failed: ${formatCause(cause)}`,
					cause,
					status,
				}),
		),
	);
}

function decodeGitHubEmailResponseEffect(payload: unknown, status: number) {
	return Schema.decodeUnknownEffect(GitHubEmailResponseSchema)(payload).pipe(
		Effect.mapError(
			(cause) =>
				new GitHubApiError({
					operation: "fetchIdentity.emails",
					message: `GitHub response validation failed: ${formatCause(cause)}`,
					cause,
					status,
				}),
		),
	);
}

function readOptionalDate(value: number | undefined): Date | undefined {
	if (value === undefined || !Number.isFinite(value) || value <= 0) {
		return undefined;
	}
	return new Date(Date.now() + value * 1000);
}

function formatCause(cause: unknown): string {
	if (cause && typeof cause === "object" && "message" in cause) {
		return String(cause.message);
	}
	return "unknown error";
}
