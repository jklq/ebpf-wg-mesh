import { Buffer } from "node:buffer";
import { describe, expect, it } from "vitest";

import {
	createGitHubTokenCipher,
	decodeGitHubTokenEncryptionKey,
} from "#/lib/dashboard/store/token-crypto.server";

describe("GitHub OAuth token encryption", () => {
	const key = Buffer.alloc(32, 7);
	const binding = {
		userID: "user-1",
		providerSubject: "github-42",
		kind: "access" as const,
	};

	it("uses authenticated encryption with a fresh nonce per value", () => {
		const cipher = createGitHubTokenCipher(key);
		const first = cipher.encrypt("sensitive-token", binding);
		const second = cipher.encrypt("sensitive-token", binding);

		expect(first).not.toBe(second);
		expect(first).not.toContain("sensitive-token");
		expect(cipher.decrypt(first, binding)).toBe("sensitive-token");
		expect(cipher.decrypt(second, binding)).toBe("sensitive-token");
	});

	it("rejects token swapping across accounts and token kinds", () => {
		const cipher = createGitHubTokenCipher(key);
		const encrypted = cipher.encrypt("sensitive-token", binding);

		expect(() =>
			cipher.decrypt(encrypted, { ...binding, userID: "user-2" }),
		).toThrow("GitHub OAuth token decryption failed");
		expect(() =>
			cipher.decrypt(encrypted, { ...binding, kind: "refresh" }),
		).toThrow("GitHub OAuth token decryption failed");
	});

	it("does not include token material in authentication errors", () => {
		const cipher = createGitHubTokenCipher(key);
		const encrypted = cipher.encrypt("never-log-this-token", binding);
		// Flip ciphertext bytes; replacing the final base64 character can leave
		// the payload unchanged because padding bits are ignored.
		const [prefix, encoded] = encrypted.split(".");
		const payload = Buffer.from(encoded, "base64url");
		payload[payload.length - 1] ^= 1;
		const tampered = `${prefix}.${payload.toString("base64url")}`;

		expect(() => cipher.decrypt(tampered, binding)).toThrowError(
			new Error("GitHub OAuth token decryption failed"),
		);
	});

	it("accepts standard and URL-safe encodings of exactly 32 bytes", () => {
		expect(decodeGitHubTokenEncryptionKey(key.toString("base64"))).toEqual(key);
		expect(decodeGitHubTokenEncryptionKey(key.toString("base64url"))).toEqual(
			key,
		);
		expect(() =>
			decodeGitHubTokenEncryptionKey(Buffer.alloc(31).toString("base64")),
		).toThrow("must encode exactly 32 bytes");
	});
});
