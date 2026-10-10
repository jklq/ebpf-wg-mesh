import { Buffer } from "node:buffer";
import type { Pool } from "pg";
import { describe, expect, it } from "vitest";

import { migrateDashboardStore } from "#/lib/dashboard/store/helpers.server";
import { createGitHubTokenCipher } from "#/lib/dashboard/store/token-crypto.server";
import type { DashboardStoreRuntimeConfig } from "#/lib/dashboard/store/types.server";

describe("restored console startup", () => {
	function fixture(generation = "recovery", versions = ["3"], paused = true) {
		const statements: string[] = [];
		const runtime: DashboardStoreRuntimeConfig = {
			installationID: "installation",
			recoveryGeneration: "recovery",
			recoveryPaused: false,
			databaseSchema: "dashboard",
			githubTokenCipher: createGitHubTokenCipher(Buffer.alloc(32, 4)),
		};
		const db = {
			async query(text: string) {
				statements.push(text);
				if (text.includes("recovery_runtime_authority")) {
					return {
						rows: [{ installation: "installation", generation, paused }],
					};
				}
				if (text.includes("schema_migrations")) {
					if (text.includes("information_schema")) {
						return { rows: [{ exists: false }] };
					}
					return {
						rows: versions.map((version) => ({
							version,
							compatible_from: version,
						})),
					};
				}
				throw new Error("restored startup attempted a schema mutation");
			},
		} as unknown as Pool;
		return { runtime, db, statements };
	}

	it("uses the shared pause and inspects the existing schema without initialization", async () => {
		const { runtime, db, statements } = fixture();
		await migrateDashboardStore(runtime, db);
		expect(runtime.recoveryPaused).toBe(true);
		expect(statements).toHaveLength(3);
		expect(statements.every((sql) => sql.startsWith("SELECT"))).toBe(true);
	});

	it("verifies the installer-owned schema on an unpaused production restart", async () => {
		const { runtime, db, statements } = fixture("recovery", ["3"], false);
		await migrateDashboardStore(runtime, db);
		expect(runtime.recoveryPaused).toBe(false);
		expect(statements).toHaveLength(3);
		expect(statements.every((sql) => sql.startsWith("SELECT"))).toBe(true);
	});

	it("rejects prior-generation admission before touching the console schema", async () => {
		const { runtime, db, statements } = fixture("before");
		await expect(migrateDashboardStore(runtime, db)).rejects.toThrow(
			"console admission differs",
		);
		expect(statements).toHaveLength(1);
	});

	it("rejects an incomplete restored schema without creating it", async () => {
		const { runtime, db, statements } = fixture("recovery", []);
		await expect(migrateDashboardStore(runtime, db)).rejects.toThrow(
			"console schema is incompatible",
		);
		expect(statements.every((sql) => sql.startsWith("SELECT"))).toBe(true);
	});
});
