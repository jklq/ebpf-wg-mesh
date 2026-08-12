import { createHmac } from "node:crypto";
import { describe, expect, it } from "vitest";

import {
	createPlatformUserAssertion,
	PlatformUserAssertionAudience,
	PlatformUserAssertionIssuer,
	PlatformUserAssertionMaxAgeSeconds,
} from "#/lib/platform-grpc/user-assertion.server";

describe("createPlatformUserAssertion", () => {
	it("signs a short-lived control-plane assertion for the user", () => {
		const secret = "a-distinct-test-user-assertion-secret";
		const token = createPlatformUserAssertion({
			secret,
			userId: "user-1",
			now: new Date("2026-08-11T12:00:00Z"),
			id: "assertion-1",
		});
		const [headerSegment, payloadSegment, signatureSegment] = token.split(".");
		const header = JSON.parse(
			Buffer.from(headerSegment, "base64url").toString("utf8"),
		);
		const payload = JSON.parse(
			Buffer.from(payloadSegment, "base64url").toString("utf8"),
		);

		expect(header).toEqual({ alg: "HS256", typ: "JWT" });
		expect(payload).toEqual({
			iss: PlatformUserAssertionIssuer,
			aud: PlatformUserAssertionAudience,
			sub: "user-1",
			iat: 1_786_449_600,
			exp: 1_786_449_600 + PlatformUserAssertionMaxAgeSeconds,
			jti: "assertion-1",
		});
		expect(signatureSegment).toBe(
			createHmac("sha256", secret)
				.update(`${headerSegment}.${payloadSegment}`)
				.digest("base64url"),
		);
	});

	it("rejects weak keys and invalid subjects", () => {
		expect(() =>
			createPlatformUserAssertion({ secret: "short", userId: "user-1" }),
		).toThrow("at least 32 bytes");
		expect(() =>
			createPlatformUserAssertion({
				secret: "a-distinct-test-user-assertion-secret",
				userId: " user-1",
			}),
		).toThrow("invalid user assertion subject");
	});
});
