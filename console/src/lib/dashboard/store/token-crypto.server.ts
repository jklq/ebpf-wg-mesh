import { Buffer } from "node:buffer";
import { createCipheriv, createDecipheriv, randomBytes } from "node:crypto";

const ciphertextPrefix = "ghe1.";
const nonceLength = 12;
const tagLength = 16;

export type GitHubTokenKind = "access" | "refresh";

export interface GitHubTokenCipher {
	encrypt(
		plaintext: string,
		binding: { userID: string; providerSubject: string; kind: GitHubTokenKind },
	): string;
	decrypt(
		ciphertext: string,
		binding: { userID: string; providerSubject: string; kind: GitHubTokenKind },
	): string;
	isEncrypted(value: string): boolean;
}

export function decodeGitHubTokenEncryptionKey(value: string): Buffer {
	const encoded = value.trim();
	if (!/^[A-Za-z0-9+/_-]+={0,2}$/.test(encoded)) {
		throw new Error("GitHub OAuth token encryption key is not valid base64");
	}
	let key: Buffer;
	try {
		key = Buffer.from(encoded, "base64");
	} catch {
		throw new Error("GitHub OAuth token encryption key is not valid base64");
	}
	const normalizedInput = encoded
		.replace(/-/g, "+")
		.replace(/_/g, "/")
		.replace(/=+$/, "");
	if (
		key.length !== 32 ||
		key.toString("base64").replace(/=+$/, "") !== normalizedInput
	) {
		throw new Error(
			"GitHub OAuth token encryption key must encode exactly 32 bytes",
		);
	}
	return key;
}

export function createGitHubTokenCipher(key: Uint8Array): GitHubTokenCipher {
	if (key.byteLength !== 32) {
		throw new Error(
			"GitHub OAuth token encryption key must be exactly 32 bytes",
		);
	}
	const secretKey = Buffer.from(key);
	return {
		encrypt(plaintext, binding) {
			if (!plaintext) return "";
			const nonce = randomBytes(nonceLength);
			const cipher = createCipheriv("aes-256-gcm", secretKey, nonce);
			cipher.setAAD(additionalData(binding));
			const encrypted = Buffer.concat([
				cipher.update(plaintext, "utf8"),
				cipher.final(),
			]);
			return `${ciphertextPrefix}${Buffer.concat([
				nonce,
				encrypted,
				cipher.getAuthTag(),
			]).toString("base64url")}`;
		},
		decrypt(ciphertext, binding) {
			if (!ciphertext) return "";
			try {
				if (!ciphertext.startsWith(ciphertextPrefix)) throw new Error();
				const payload = Buffer.from(
					ciphertext.slice(ciphertextPrefix.length),
					"base64url",
				);
				if (payload.length <= nonceLength + tagLength) throw new Error();
				const nonce = payload.subarray(0, nonceLength);
				const encrypted = payload.subarray(nonceLength, -tagLength);
				const tag = payload.subarray(-tagLength);
				const decipher = createDecipheriv("aes-256-gcm", secretKey, nonce);
				decipher.setAAD(additionalData(binding));
				decipher.setAuthTag(tag);
				return Buffer.concat([
					decipher.update(encrypted),
					decipher.final(),
				]).toString("utf8");
			} catch {
				throw new Error("GitHub OAuth token decryption failed");
			}
		},
		isEncrypted(value) {
			return value === "" || value.startsWith(ciphertextPrefix);
		},
	};
}

function additionalData(binding: {
	userID: string;
	providerSubject: string;
	kind: GitHubTokenKind;
}): Buffer {
	return Buffer.from(
		`github-oauth-token:v1\0${binding.userID}\0${binding.providerSubject}\0${binding.kind}`,
		"utf8",
	);
}
