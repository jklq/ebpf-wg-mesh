import { expect, it } from "vitest";
import {
	createSessionTokenPair,
	verifyAccessToken,
	verifyRefreshToken,
} from "./jwt.server";

it("accepts the previous session key during overlap and signs only with the active key", () => {
	const now = new Date();
	const user = { id: "user", email: "user@example.test" };
	const old = { jwtSecret: "old-secret" };
	const active = { jwtSecret: "new-secret" };
	const overlap = { ...active, jwtSecretPrevious: old.jwtSecret };
	const prior = createSessionTokenPair(old, user, "session", now);
	expect(verifyAccessToken(prior.accessToken, overlap, now).user).toEqual(user);
	expect(verifyRefreshToken(prior.refreshToken, overlap, now).sessionId).toBe(
		"session",
	);
	expect(() => verifyAccessToken(prior.accessToken, active, now)).toThrow(
		"signature mismatch",
	);
	const next = createSessionTokenPair(overlap, user, "session", now);
	expect(verifyAccessToken(next.accessToken, active, now).user).toEqual(user);
	expect(() => verifyAccessToken(next.accessToken, old, now)).toThrow(
		"signature mismatch",
	);
});

it("invalidates access and refresh sessions across recovery generations even with signing-key overlap", () => {
	const now = new Date();
	const user = { id: "user", email: "user@example.test" };
	const old = {
		jwtSecret: "old-secret",
		installationID: "installation",
		recoveryGeneration: "before",
	};
	const current = {
		jwtSecret: "new-secret",
		jwtSecretPrevious: old.jwtSecret,
		installationID: old.installationID,
		recoveryGeneration: "after",
	};
	const prior = createSessionTokenPair(old, user, "session", now);
	expect(() => verifyAccessToken(prior.accessToken, current, now)).toThrow(
		"generation mismatch",
	);
	expect(() => verifyRefreshToken(prior.refreshToken, current, now)).toThrow(
		"generation mismatch",
	);
	const next = createSessionTokenPair(current, user, "new-session", now);
	expect(verifyAccessToken(next.accessToken, current, now).user).toEqual(user);
});
