import {
	createSessionTokenPair,
	decodeRefreshTokenWithoutExpiryCheck,
	verifyAccessToken,
	verifyRefreshToken,
} from "#/lib/dashboard/core/jwt.server";
import {
	type DashboardRuntime,
	githubCall,
	storeAuthCall,
	storeCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	AuthConflictError,
	AuthenticationRequiredError,
	type DashboardConfig,
	type DashboardSession,
	type DashboardUser,
	DashboardValidationError,
	type SessionCookieOptions,
	type SessionCookies,
} from "#/lib/dashboard/core/types.server";
import {
	sanitizeRedirect,
	shouldUseSecureCookies,
} from "#/lib/dashboard/core/utils.server";

interface AuthStateCookie {
	state: string;
	redirectTo: string;
}

export async function beginGitHubLogin(
	runtime: DashboardRuntime,
	input: { redirectTo?: string },
): Promise<string> {
	const { config, cookies, github } = runtime;
	if (!config.github || !github) {
		throw new AuthConflictError({
			code: "github_auth_unavailable",
			message: "GitHub sign-in is not configured",
		});
	}

	const state = runtime.randomUUID();
	const redirectTo = sanitizeRedirect(input.redirectTo);
	cookies.set(
		config.authStateCookieName,
		JSON.stringify({ state, redirectTo } satisfies AuthStateCookie),
		authStateCookieOptions(config, runtime.now()),
	);
	return github.buildAuthorizationURL({
		redirectURI: authCallbackURL(config),
		state,
	});
}

export async function completeAuthCallback(
	runtime: DashboardRuntime,
	input: {
		code?: string;
		state?: string;
		userId?: string;
		email?: string;
		redirectTo?: string;
	},
): Promise<string> {
	const { config, cookies, github } = runtime;

	if (input.code) {
		if (!config.github || !github) {
			throw new AuthConflictError({
				code: "github_auth_unavailable",
				message: "GitHub sign-in is not configured",
			});
		}

		const rawState = cookies.get(config.authStateCookieName);
		cookies.delete(config.authStateCookieName, { path: "/auth" });
		if (!rawState) {
			throw new AuthConflictError({
				code: "invalid_signin_state",
				message: "missing sign-in state cookie",
			});
		}

		const cookieState = parseAuthStateCookie(rawState);
		if (cookieState.state !== input.state) {
			throw new AuthConflictError({
				code: "invalid_signin_state",
				message: "sign-in state mismatch",
			});
		}

		const token = await githubCall(runtime, "exchangeCode", () =>
			github.exchangeCode({
				code: input.code ?? "",
				redirectURI: authCallbackURL(config),
			}),
		);
		const identity = await githubCall(runtime, "fetchIdentity", () =>
			github.fetchIdentity(token.accessToken),
		);
		const operatorGitHubLogin = config.operatorGitHubLogin
			?.trim()
			.toLowerCase();
		const operatorUserID =
			operatorGitHubLogin &&
			identity.login.toLowerCase() === operatorGitHubLogin
				? `github:${operatorGitHubLogin}`
				: undefined;
		const result = await storeAuthCall(
			runtime,
			"completeGitHubLogin",
			(store) =>
				store.completeGitHubLogin({
					userID: operatorUserID,
					providerSubject: identity.providerSubject,
					login: identity.login,
					primaryEmail: identity.primaryEmail,
					accessToken: token.accessToken,
					tokenType: token.tokenType,
					scope: token.scope,
					accessTokenExpiresAt: token.accessTokenExpiresAt,
					refreshToken: token.refreshToken,
					refreshTokenExpiresAt: token.refreshTokenExpiresAt,
				}),
		);
		return signIn(runtime, result.user, "/");
	}

	const userID = input.userId?.trim() ?? "";
	const email = input.email?.trim() ?? "";
	if (userID === "" || email === "") {
		throw new DashboardValidationError({
			message: "auth callback requires GitHub code or dev login user ID/email",
		});
	}
	if (config.devUsers.length === 0) {
		throw new AuthConflictError({
			code: "dev_auth_unavailable",
			message: "dev login is not configured",
		});
	}
	const allowed = config.devUsers.some(
		(entry) => entry.id === userID && entry.email === email,
	);
	if (!allowed) {
		throw new AuthConflictError({
			code: "invalid_dev_login",
			message: "dev login is not allowed",
		});
	}

	const user = await storeCall(runtime, "upsertDevUser", (store) =>
		store.upsertDevUser(userID, email),
	);
	return signIn(runtime, user, input.redirectTo);
}

export async function clearSession(runtime: DashboardRuntime): Promise<void> {
	const { config, cookies } = runtime;
	const refreshToken = cookies.get(config.refreshCookieName);
	if (refreshToken) {
		const refresh = readRefreshToken(config, refreshToken);
		if (refresh) {
			await storeCall(runtime, "deleteRefreshSession", (store) =>
				store.deleteRefreshSession(refresh.sessionId),
			);
		}
	}
	clearAuthCookies(cookies, config);
}

async function signIn(
	runtime: DashboardRuntime,
	user: DashboardUser,
	redirectTo?: string,
): Promise<string> {
	const { config, cookies } = runtime;
	const now = runtime.now();
	const refreshSessionId = runtime.randomUUID();
	const tokens = createSessionTokenPair(config, user, refreshSessionId, now);
	await storeCall(runtime, "createRefreshSession", (store) =>
		store.createRefreshSession(
			tokens.refreshSessionId,
			user.id,
			tokens.refreshTokenExpiresAt,
		),
	);
	cookies.set(
		config.sessionCookieName,
		tokens.accessToken,
		sessionCookieOptions(config, tokens.accessTokenExpiresAt),
	);
	cookies.set(
		config.refreshCookieName,
		tokens.refreshToken,
		refreshCookieOptions(config, tokens.refreshTokenExpiresAt),
	);
	return sanitizeRedirect(redirectTo);
}

export async function currentSession(
	runtime: DashboardRuntime,
): Promise<DashboardSession | null> {
	const { config, cookies } = runtime;
	const now = runtime.now();
	const accessToken = cookies.get(config.sessionCookieName);
	if (accessToken) {
		const access = readAccessToken(config, accessToken, now);
		if (access) {
			return { user: access.user };
		}
	}
	return refreshSessionFromCookies(runtime);
}

export async function refreshSession(runtime: DashboardRuntime): Promise<void> {
	const session = await refreshSessionFromCookies(runtime);
	if (!session) {
		throw new AuthenticationRequiredError({
			message: "authentication required",
		});
	}
}

export async function requireSession(
	runtime: DashboardRuntime,
): Promise<DashboardSession> {
	const session = await currentSession(runtime);
	if (!session) {
		throw new AuthenticationRequiredError({
			message: "authentication required",
		});
	}
	return session;
}

export function sessionCookieOptions(
	config: DashboardConfig,
	expiresAt: Date,
): SessionCookieOptions {
	return {
		httpOnly: true,
		path: "/",
		sameSite: "lax",
		secure: shouldUseSecureCookies(config),
		expires: expiresAt,
	};
}

export function refreshCookieOptions(
	config: DashboardConfig,
	expiresAt: Date,
): SessionCookieOptions {
	return {
		httpOnly: true,
		path: "/",
		sameSite: "lax",
		secure: shouldUseSecureCookies(config),
		expires: expiresAt,
	};
}

export function authStateCookieOptions(
	config: DashboardConfig,
	currentTime: Date,
): SessionCookieOptions {
	return {
		httpOnly: true,
		path: "/auth",
		sameSite: "lax",
		secure: shouldUseSecureCookies(config),
		expires: new Date(currentTime.getTime() + 10 * 60 * 1000),
	};
}

export async function refreshSessionFromCookies(
	runtime: DashboardRuntime,
): Promise<DashboardSession | null> {
	const { config, cookies } = runtime;
	const now = runtime.now();
	const refreshToken = cookies.get(config.refreshCookieName);
	if (!refreshToken) {
		clearAuthCookies(cookies, config);
		return null;
	}

	const refresh = readRefreshToken(config, refreshToken, now);
	if (!refresh) {
		clearAuthCookies(cookies, config);
		return null;
	}

	const nextSessionId = runtime.randomUUID();
	const nextRefreshExpiresAt = new Date(
		now.getTime() + config.sessionMaxAgeSeconds * 1000,
	);
	const user = await storeCall(runtime, "rotateRefreshSession", (store) =>
		store.rotateRefreshSession({
			sessionId: refresh.sessionId,
			userID: refresh.user.id,
			now,
			nextSessionId,
			expiresAt: nextRefreshExpiresAt,
		}),
	);
	if (!user) {
		clearAuthCookies(cookies, config);
		return null;
	}

	const tokens = createSessionTokenPair(config, user, nextSessionId, now);
	cookies.set(
		config.sessionCookieName,
		tokens.accessToken,
		sessionCookieOptions(config, tokens.accessTokenExpiresAt),
	);
	cookies.set(
		config.refreshCookieName,
		tokens.refreshToken,
		refreshCookieOptions(config, tokens.refreshTokenExpiresAt),
	);
	return { user };
}

function clearAuthCookies(cookies: SessionCookies, config: DashboardConfig) {
	cookies.delete(config.sessionCookieName, { path: "/" });
	cookies.delete(config.refreshCookieName, { path: "/" });
}

function readAccessToken(config: DashboardConfig, token: string, now: Date) {
	try {
		return verifyAccessToken(token, config, now);
	} catch {
		return null;
	}
}

function readRefreshToken(config: DashboardConfig, token: string, now?: Date) {
	try {
		return now
			? verifyRefreshToken(token, config, now)
			: decodeRefreshTokenWithoutExpiryCheck(token, config);
	} catch {
		return null;
	}
}

function authCallbackURL(config: DashboardConfig): string {
	return `${config.publicBaseURL.replace(/\/$/, "")}/auth/callback`;
}

function parseAuthStateCookie(rawState: string): AuthStateCookie {
	try {
		const parsed = JSON.parse(rawState);
		if (
			parsed &&
			typeof parsed === "object" &&
			typeof parsed.state === "string" &&
			parsed.state.trim() !== "" &&
			typeof parsed.redirectTo === "string"
		) {
			return {
				state: parsed.state,
				redirectTo: parsed.redirectTo,
			};
		}
	} catch {
		// Fall through.
	}
	throw new AuthConflictError({
		code: "invalid_signin_state",
		message: "invalid sign-in state cookie",
	});
}
