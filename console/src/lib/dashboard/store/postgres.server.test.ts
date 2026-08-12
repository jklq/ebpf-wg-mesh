import { Buffer } from "node:buffer";
import type { Pool } from "pg";
import { describe, expect, it } from "vitest";

import { createPostgresDashboardStore } from "#/lib/dashboard/store/postgres.server";
import { createGitHubTokenCipher } from "#/lib/dashboard/store/token-crypto.server";

describe("Postgres dashboard GitHub token store", () => {
	it("persists only ciphertext and decrypts account reads", async () => {
		const cipher = createGitHubTokenCipher(Buffer.alloc(32, 4));
		const row = {
			user_id: "user-1",
			provider_subject: "github-1",
			verified_email_snapshot: "user@example.test",
			provider_login: "octocat",
			access_token: "",
			access_token_expires_at: null,
			refresh_token: "",
			refresh_token_expires_at: null,
			token_type: "bearer",
			scope: "",
			oauth_token_version: 0,
		};
		const pool = {
			async query(text: string, values: ReadonlyArray<unknown> = []) {
				if (
					text.includes("UPDATE dashboard.accounts") &&
					text.includes("oauth_token_version = oauth_token_version + 1")
				) {
					row.access_token = String(values[4]);
					row.access_token_expires_at = values[5] as null;
					row.refresh_token = String(values[6]);
					row.refresh_token_expires_at = values[7] as null;
					row.token_type = String(values[8]);
					row.scope = String(values[9]);
					row.oauth_token_version += 1;
					return { rowCount: 1, rows: [{ ...row }] };
				}
				if (
					text.includes("SELECT user_id") &&
					text.includes("provider_subject")
				) {
					return { rowCount: 1, rows: [{ ...row }] };
				}
				throw new Error("unexpected test query");
			},
		} as unknown as Pool;
		const store = createPostgresDashboardStore(
			{ databaseSchema: "dashboard", githubTokenCipher: cipher },
			pool,
		);

		const refreshed = await store.completeGitHubTokenRefresh({
			userID: "user-1",
			providerSubject: "github-1",
			expectedTokenVersion: 0,
			leaseID: "lease-1",
			token: {
				accessToken: "plain-access-token",
				refreshToken: "plain-refresh-token",
				tokenType: "bearer",
				scope: "repo",
			},
			fallbackRefreshToken: "old-refresh-token",
		});

		expect(row.access_token).toMatch(/^ghe1\./);
		expect(row.refresh_token).toMatch(/^ghe1\./);
		expect(JSON.stringify(row)).not.toContain("plain-access-token");
		expect(JSON.stringify(row)).not.toContain("plain-refresh-token");
		expect(refreshed).toMatchObject({
			accessToken: "plain-access-token",
			refreshToken: "plain-refresh-token",
			tokenVersion: 1,
		});
		await expect(store.getGitHubAccount("user-1")).resolves.toMatchObject({
			accessToken: "plain-access-token",
			refreshToken: "plain-refresh-token",
			tokenVersion: 1,
		});
	});
});
