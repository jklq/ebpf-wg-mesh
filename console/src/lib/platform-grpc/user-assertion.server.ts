import { createHmac, randomUUID } from "node:crypto";

export const PlatformUserAssertionIssuer = "managed-dashboard";
export const PlatformUserAssertionAudience = "controlplane";
export const PlatformUserAssertionMaxAgeSeconds = 30;

const header = {
	alg: "HS256",
	typ: "JWT",
} as const;

interface PlatformUserAssertionPayload {
	iss: typeof PlatformUserAssertionIssuer;
	aud: typeof PlatformUserAssertionAudience;
	sub: string;
	iat: number;
	exp: number;
	jti: string;
}

export function createPlatformUserAssertion(input: {
	secret: string;
	userId: string;
	now?: Date;
	id?: string;
}): string {
	if (Buffer.byteLength(input.secret, "utf8") < 32) {
		throw new Error("user assertion secret must be at least 32 bytes");
	}
	if (
		input.userId.length === 0 ||
		input.userId.trim() !== input.userId ||
		Buffer.byteLength(input.userId, "utf8") > 256
	) {
		throw new Error("invalid user assertion subject");
	}
	const now = Math.floor((input.now ?? new Date()).getTime() / 1000);
	const payload: PlatformUserAssertionPayload = {
		iss: PlatformUserAssertionIssuer,
		aud: PlatformUserAssertionAudience,
		sub: input.userId,
		iat: now,
		exp: now + PlatformUserAssertionMaxAgeSeconds,
		jti: input.id ?? randomUUID(),
	};
	const signingInput = `${encodeJSON(header)}.${encodeJSON(payload)}`;
	const signature = createHmac("sha256", input.secret)
		.update(signingInput)
		.digest("base64url");
	return `${signingInput}.${signature}`;
}

function encodeJSON(value: unknown): string {
	return Buffer.from(JSON.stringify(value), "utf8").toString("base64url");
}
