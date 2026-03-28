import {
	AuthConflictError,
	GitHubApiError,
	type GitHubAppUserAuthConfig,
	type GitHubAppUserClient,
	type GitHubAppUserIdentity,
	type GitHubAppUserToken,
	type GitHubUserRepository,
} from "#/lib/dashboard-core.server";

interface GitHubTokenResponse {
	access_token: string;
	token_type: string;
	scope?: string;
	expires_in?: number;
	refresh_token?: string;
	refresh_token_expires_in?: number;
}

interface GitHubUserResponse {
	id: string | number;
	login: string;
}

interface GitHubEmailResponseEntry {
	email: string;
	primary: boolean;
	verified: boolean;
}

interface GitHubRepositoryResponseEntry {
	name: string;
	full_name: string;
	private: boolean;
	default_branch?: string;
	owner: {
		login: string;
	};
}

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
			return exchangeCode(config, input);
		},

		fetchIdentity(accessToken) {
			return fetchIdentity(config, accessToken);
		},

		listRepositories(accessToken) {
			return listRepositories(config, accessToken);
		},
	};
}

async function exchangeCode(
	config: GitHubAppUserAuthConfig,
	input: {
		code: string;
		redirectURI: string;
	},
): Promise<GitHubAppUserToken> {
	const tokenURL = new URL(
		"/login/oauth/access_token",
		config.authorizationBaseURL,
	);
	tokenURL.searchParams.set("client_id", config.clientId);
	tokenURL.searchParams.set("client_secret", config.clientSecret);
	tokenURL.searchParams.set("code", input.code);
	tokenURL.searchParams.set("redirect_uri", input.redirectURI);

	const response = await fetchGitHub("exchangeCode", tokenURL, {
		method: "POST",
		headers: {
			Accept: "application/json",
			"User-Agent": `ebpf-wg-mesh-github-app/${config.appId}`,
		},
	});
	const payload = decodeGitHubTokenResponse(
		await readJson("exchangeCode.response", response),
		response.status,
	);

	return {
		accessToken: payload.access_token,
		tokenType: payload.token_type,
		scope: payload.scope ?? "",
		accessTokenExpiresAt: readOptionalDate(payload.expires_in),
		refreshToken: payload.refresh_token,
		refreshTokenExpiresAt: readOptionalDate(payload.refresh_token_expires_in),
	};
}

async function fetchIdentity(
	config: GitHubAppUserAuthConfig,
	accessToken: string,
): Promise<GitHubAppUserIdentity> {
	const userResponse = await githubGET(config, "/user", accessToken);
	const user = decodeGitHubUserResponse(
		await readJson("fetchIdentity.user", userResponse),
		userResponse.status,
	);

	const emailsResponse = await githubGET(config, "/user/emails", accessToken);
	const emails = decodeGitHubEmailResponse(
		await readJson("fetchIdentity.emails", emailsResponse),
		emailsResponse.status,
	);

	const primaryVerified = emails.find(
		(entry) => entry.primary === true && entry.verified === true,
	);
	if (!primaryVerified) {
		throw new AuthConflictError({
			code: "missing_verified_email",
			message: "GitHub account does not expose a verified primary email",
		});
	}

	return {
		providerSubject: String(user.id),
		login: user.login,
		primaryEmail: primaryVerified.email,
	};
}

async function listRepositories(
	config: GitHubAppUserAuthConfig,
	accessToken: string,
): Promise<Array<GitHubUserRepository>> {
	const repositories: Array<GitHubUserRepository> = [];

	for (let page = 1; page <= 5; page += 1) {
		const url = new URL("/user/repos", config.apiBaseURL);
		url.searchParams.set("affiliation", "owner");
		url.searchParams.set("sort", "updated");
		url.searchParams.set("per_page", "100");
		url.searchParams.set("page", String(page));

		const response = await fetchGitHub(`listRepositories:${page}`, url, {
			headers: {
				Accept: "application/vnd.github+json",
				Authorization: `Bearer ${accessToken}`,
				"User-Agent": `ebpf-wg-mesh-github-app/${config.appId}`,
				"X-GitHub-Api-Version": "2022-11-28",
			},
		});
		const payload = decodeGitHubRepositoryResponse(
			await readJson(`listRepositories.${page}`, response),
			`listRepositories.${page}`,
			response.status,
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
}

function githubGET(
	config: GitHubAppUserAuthConfig,
	path: string,
	accessToken: string,
): Promise<Response> {
	const url = new URL(path, config.apiBaseURL);
	return fetchGitHub(`githubGET:${path}`, url, {
		headers: {
			Accept: "application/vnd.github+json",
			Authorization: `Bearer ${accessToken}`,
			"User-Agent": `ebpf-wg-mesh-github-app/${config.appId}`,
			"X-GitHub-Api-Version": "2022-11-28",
		},
	});
}

async function fetchGitHub(
	operation: string,
	url: URL,
	init: RequestInit,
): Promise<Response> {
	let response: Response;
	try {
		response = await fetch(url, init);
	} catch (cause) {
		throw new GitHubApiError({
			operation,
			message: formatCause(cause),
			cause,
		});
	}

	if (!response.ok) {
		throw new GitHubApiError({
			operation,
			message: `GitHub request failed: ${response.status}`,
			cause: response,
			status: response.status,
		});
	}

	return response;
}

async function readJson(operation: string, response: Response): Promise<unknown> {
	try {
		return await response.json();
	} catch (cause) {
		throw new GitHubApiError({
			operation,
			message: `GitHub JSON decode failed: ${formatCause(cause)}`,
			cause,
			status: response.status,
		});
	}
}

function decodeGitHubTokenResponse(
	payload: unknown,
	status: number,
): GitHubTokenResponse {
	const value = readObject(payload, "exchangeCode.response", status);
	return {
		access_token: readRequiredString(
			value.access_token,
			"access_token",
			"exchangeCode.response",
			status,
		),
		token_type: readRequiredString(
			value.token_type,
			"token_type",
			"exchangeCode.response",
			status,
		),
		scope: readOptionalString(value.scope),
		expires_in: readOptionalNumber(value.expires_in),
		refresh_token: readOptionalString(value.refresh_token),
		refresh_token_expires_in: readOptionalNumber(
			value.refresh_token_expires_in,
		),
	};
}

function decodeGitHubUserResponse(
	payload: unknown,
	status: number,
): GitHubUserResponse {
	const value = readObject(payload, "fetchIdentity.user", status);
	const id = value.id;
	if (typeof id !== "string" && typeof id !== "number") {
		throw validationError(
			"fetchIdentity.user",
			"invalid field id",
			payload,
			status,
		);
	}

	return {
		id,
		login: readRequiredString(value.login, "login", "fetchIdentity.user", status),
	};
}

function decodeGitHubEmailResponse(
	payload: unknown,
	status: number,
): Array<GitHubEmailResponseEntry> {
	if (!Array.isArray(payload)) {
		throw validationError(
			"fetchIdentity.emails",
			"expected an array",
			payload,
			status,
		);
	}

	return payload.map((entry) => {
		const value = readObject(entry, "fetchIdentity.emails", status);
		return {
			email: readRequiredString(
				value.email,
				"email",
				"fetchIdentity.emails",
				status,
			),
			primary: readRequiredBoolean(
				value.primary,
				"primary",
				"fetchIdentity.emails",
				status,
			),
			verified: readRequiredBoolean(
				value.verified,
				"verified",
				"fetchIdentity.emails",
				status,
			),
		};
	});
}

function decodeGitHubRepositoryResponse(
	payload: unknown,
	operation: string,
	status: number,
): Array<GitHubRepositoryResponseEntry> {
	if (!Array.isArray(payload)) {
		throw validationError(operation, "expected an array", payload, status);
	}

	return payload.map((entry) => {
		const value = readObject(entry, operation, status);
		const owner = readObject(value.owner, operation, status);
		return {
			name: readRequiredString(value.name, "name", operation, status),
			full_name: readRequiredString(
				value.full_name,
				"full_name",
				operation,
				status,
			),
			private: readRequiredBoolean(value.private, "private", operation, status),
			default_branch: readOptionalString(value.default_branch),
			owner: {
				login: readRequiredString(owner.login, "owner.login", operation, status),
			},
		};
	});
}

function readObject(
	value: unknown,
	operation: string,
	status: number,
): Record<string, unknown> {
	if (!value || typeof value !== "object" || Array.isArray(value)) {
		throw validationError(operation, "expected an object", value, status);
	}
	return value as Record<string, unknown>;
}

function readRequiredString(
	value: unknown,
	field: string,
	operation: string,
	status: number,
): string {
	if (typeof value !== "string" || value.length === 0) {
		throw validationError(operation, `invalid field ${field}`, value, status);
	}
	return value;
}

function readRequiredBoolean(
	value: unknown,
	field: string,
	operation: string,
	status: number,
): boolean {
	if (typeof value !== "boolean") {
		throw validationError(operation, `invalid field ${field}`, value, status);
	}
	return value;
}

function readOptionalString(value: unknown): string | undefined {
	return typeof value === "string" ? value : undefined;
}

function readOptionalNumber(value: unknown): number | undefined {
	return typeof value === "number" ? value : undefined;
}

function validationError(
	operation: string,
	message: string,
	cause: unknown,
	status: number,
): GitHubApiError {
	return new GitHubApiError({
		operation,
		message: `GitHub response validation failed: ${message}`,
		cause,
		status,
	});
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
