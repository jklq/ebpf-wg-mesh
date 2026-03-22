import { createHmac, timingSafeEqual } from "node:crypto";

import type { DashboardUser } from "#/lib/dashboard-core.server";

export const DashboardAccessTokenMaxAgeSeconds = 60 * 60;
export const DashboardRefreshTokenMaxAgeSeconds = 30 * 24 * 60 * 60;

const DashboardJWTAlgorithm = "HS256";
const DashboardJWTHeader = {
	alg: DashboardJWTAlgorithm,
	typ: "JWT",
} as const;
const DashboardJWTIssuer = "managed-dashboard";
const DashboardJWTAudience = "managed-dashboard";

export interface DashboardJWTConfig {
	jwtSecret: string;
}

interface DashboardTokenPayload {
	iss: string;
	aud: string;
	typ: "access" | "refresh";
	sub: string;
	uid: string;
	email: string;
	iat: number;
	exp: number;
	sid?: string;
}

export interface DashboardVerifiedAccessToken {
	user: DashboardUser;
	expiresAt: Date;
}

export interface DashboardVerifiedRefreshToken {
	user: DashboardUser;
	sessionId: string;
	expiresAt: Date;
}

export interface DashboardSessionTokenPair {
	accessToken: string;
	accessTokenExpiresAt: Date;
	refreshToken: string;
	refreshTokenExpiresAt: Date;
	refreshSessionId: string;
}

export function createSessionTokenPair(
	config: DashboardJWTConfig,
	user: DashboardUser,
	refreshSessionId: string,
	now: Date,
): DashboardSessionTokenPair {
	const accessTokenExpiresAt = new Date(
		now.getTime() + DashboardAccessTokenMaxAgeSeconds * 1000,
	);
	const refreshTokenExpiresAt = new Date(
		now.getTime() + DashboardRefreshTokenMaxAgeSeconds * 1000,
	);
	return {
		accessToken: signToken(config, {
			typ: "access",
			user,
			now,
			expiresAt: accessTokenExpiresAt,
			sessionId: refreshSessionId,
		}),
		accessTokenExpiresAt,
		refreshToken: signToken(config, {
			typ: "refresh",
			user,
			now,
			expiresAt: refreshTokenExpiresAt,
			sessionId: refreshSessionId,
		}),
		refreshTokenExpiresAt,
		refreshSessionId,
	};
}

export function verifyAccessToken(
	token: string,
	config: DashboardJWTConfig,
	now: Date,
): DashboardVerifiedAccessToken {
	const payload = verifyToken(token, config, "access", now);
	return {
		user: payloadUser(payload),
		expiresAt: fromUnixTime(payload.exp),
	};
}

export function verifyRefreshToken(
	token: string,
	config: DashboardJWTConfig,
	now: Date,
): DashboardVerifiedRefreshToken {
	const payload = verifyToken(token, config, "refresh", now);
	if (!payload.sid) {
		throw new Error("missing refresh session id");
	}
	return {
		user: payloadUser(payload),
		sessionId: payload.sid,
		expiresAt: fromUnixTime(payload.exp),
	};
}

export function decodeRefreshTokenWithoutExpiryCheck(
	token: string,
	config: DashboardJWTConfig,
): DashboardVerifiedRefreshToken {
	const payload = verifyToken(token, config, "refresh", new Date(), false);
	if (!payload.sid) {
		throw new Error("missing refresh session id");
	}
	return {
		user: payloadUser(payload),
		sessionId: payload.sid,
		expiresAt: fromUnixTime(payload.exp),
	};
}

function signToken(
	config: DashboardJWTConfig,
	input: {
		typ: DashboardTokenPayload["typ"];
		user: DashboardUser;
		now: Date;
		expiresAt: Date;
		sessionId?: string;
	},
): string {
	const header = encodeJSON(DashboardJWTHeader);
	const payload = encodeJSON({
		iss: DashboardJWTIssuer,
		aud: DashboardJWTAudience,
		typ: input.typ,
		sub: input.user.subject,
		uid: input.user.id,
		email: input.user.email,
		iat: toUnixTime(input.now),
		exp: toUnixTime(input.expiresAt),
		...(input.sessionId ? { sid: input.sessionId } : {}),
	} satisfies DashboardTokenPayload);
	const signingInput = `${header}.${payload}`;
	return `${signingInput}.${sign(config.jwtSecret, signingInput)}`;
}

function verifyToken(
	token: string,
	config: DashboardJWTConfig,
	expectedType: DashboardTokenPayload["typ"],
	now: Date,
	verifyExpiry = true,
): DashboardTokenPayload {
	const parts = token.split(".");
	if (parts.length !== 3) {
		throw new Error("token must contain three segments");
	}
	const [headerSegment, payloadSegment, signatureSegment] = parts;
	const signingInput = `${headerSegment}.${payloadSegment}`;
	const expectedSignature = decodeBase64URL(
		sign(config.jwtSecret, signingInput),
	);
	const actualSignature = decodeBase64URL(signatureSegment);
	if (
		expectedSignature.length !== actualSignature.length ||
		!timingSafeEqual(expectedSignature, actualSignature)
	) {
		throw new Error("token signature mismatch");
	}

	const header = decodeJSON(headerSegment);
	if (
		!header ||
		typeof header !== "object" ||
		header.alg !== DashboardJWTAlgorithm ||
		header.typ !== "JWT"
	) {
		throw new Error("invalid token header");
	}

	const payload = decodeJSON(payloadSegment);
	if (!isDashboardTokenPayload(payload, expectedType)) {
		throw new Error("invalid token payload");
	}
	if (
		payload.iss !== DashboardJWTIssuer ||
		payload.aud !== DashboardJWTAudience
	) {
		throw new Error("invalid token issuer or audience");
	}
	if (verifyExpiry && payload.exp <= toUnixTime(now)) {
		throw new Error("token expired");
	}
	return payload;
}

function sign(secret: string, value: string): string {
	return createHmac("sha256", secret).update(value).digest("base64url");
}

function payloadUser(payload: DashboardTokenPayload): DashboardUser {
	return {
		id: payload.uid,
		subject: payload.sub,
		email: payload.email,
	};
}

function encodeJSON(value: unknown): string {
	return Buffer.from(JSON.stringify(value), "utf8").toString("base64url");
}

function decodeJSON(value: string): unknown {
	return JSON.parse(Buffer.from(value, "base64url").toString("utf8"));
}

function decodeBase64URL(value: string): Buffer {
	return Buffer.from(value, "base64url");
}

function toUnixTime(date: Date): number {
	return Math.floor(date.getTime() / 1000);
}

function fromUnixTime(value: number): Date {
	return new Date(value * 1000);
}

function isDashboardTokenPayload(
	value: unknown,
	expectedType: DashboardTokenPayload["typ"],
): value is DashboardTokenPayload {
	if (!value || typeof value !== "object") {
		return false;
	}
	const payload = value as Partial<DashboardTokenPayload>;
	if (
		payload.typ !== expectedType ||
		typeof payload.iss !== "string" ||
		typeof payload.aud !== "string" ||
		typeof payload.sub !== "string" ||
		typeof payload.uid !== "string" ||
		typeof payload.email !== "string" ||
		typeof payload.iat !== "number" ||
		typeof payload.exp !== "number"
	) {
		return false;
	}
	if (expectedType === "refresh" && typeof payload.sid !== "string") {
		return false;
	}
	return true;
}
