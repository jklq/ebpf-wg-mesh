import { Buffer } from "node:buffer";
import type { Pool } from "pg";
import { describe, expect, it } from "vitest";

import { reencryptLegacyGitHubTokensBatch } from "#/lib/dashboard/store/helpers.server";
import { createGitHubTokenCipher } from "#/lib/dashboard/store/token-crypto.server";

describe("legacy GitHub OAuth token migration", () => {
	it("re-encrypts a bounded batch transactionally and leaves no plaintext writes", async () => {
		const cipher = createGitHubTokenCipher(Buffer.alloc(32, 9));
		const updates: Array<ReadonlyArray<unknown>> = [];
		let selections = 0;
		const client = {
			async query(text: string, values: ReadonlyArray<unknown> = []) {
				if (text.includes("SELECT id, user_id, provider_subject")) {
					selections += 1;
					return selections === 1
						? {
								rowCount: 1,
								rows: [
									{
										id: "account-1",
										user_id: "user-1",
										provider_subject: "github-1",
										access_token: "legacy-access",
										refresh_token: "legacy-refresh",
									},
								],
							}
						: { rowCount: 0, rows: [] };
				}
				if (text.includes("SET access_token = $2")) updates.push(values);
				return { rowCount: 0, rows: [] };
			},
			release() {},
		};
		const pool = {
			async connect() {
				return client;
			},
		} as unknown as Pool;
		const runtime = {
			databaseSchema: "dashboard",
			githubTokenCipher: cipher,
		};

		await expect(
			reencryptLegacyGitHubTokensBatch(runtime, pool, 25),
		).resolves.toBe(true);
		expect(updates).toHaveLength(1);
		const values = updates[0];
		if (!values) throw new Error("missing migration update");
		expect(values[1]).not.toBe("legacy-access");
		expect(values[2]).not.toBe("legacy-refresh");
		expect(
			cipher.decrypt(String(values[1]), {
				userID: "user-1",
				providerSubject: "github-1",
				kind: "access",
			}),
		).toBe("legacy-access");
		expect(
			cipher.decrypt(String(values[2]), {
				userID: "user-1",
				providerSubject: "github-1",
				kind: "refresh",
			}),
		).toBe("legacy-refresh");
		await expect(
			reencryptLegacyGitHubTokensBatch(runtime, pool, 25),
		).resolves.toBe(false);
	});
});
