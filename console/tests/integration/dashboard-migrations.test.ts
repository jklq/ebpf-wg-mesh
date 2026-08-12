import { randomBytes, randomUUID } from "node:crypto";
import { Pool } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { dashboardStoreMigrations } from "#/lib/dashboard/store/migrations.server";
import { createPostgresDashboardStore } from "#/lib/dashboard/store/postgres.server";
import { createGitHubTokenCipher } from "#/lib/dashboard/store/token-crypto.server";
import type { DashboardStoreRuntimeConfig } from "#/lib/dashboard/store/types.server";

const databaseURL = process.env.DASHBOARD_TEST_DATABASE_URL;
if (!databaseURL) {
	throw new Error(
		"DASHBOARD_TEST_DATABASE_URL is required; run `make test-integration-console`",
	);
}

const pool = new Pool({ connectionString: databaseURL, max: 4 });
const schemas: string[] = [];

function newRuntime(prefix: string): DashboardStoreRuntimeConfig {
	const schema = `${prefix}_${randomUUID().replaceAll("-", "")}`;
	schemas.push(schema);
	return {
		databaseSchema: schema,
		githubTokenCipher: createGitHubTokenCipher(randomBytes(32)),
	};
}

beforeAll(async () => {
	await pool.query("SELECT 1");
});

afterAll(async () => {
	for (const schema of schemas) {
		await pool.query(`DROP SCHEMA IF EXISTS ${schema} CASCADE`);
	}
	await pool.end();
});

describe("dashboard CockroachDB migrations", () => {
	it("installs the current schema from an empty database and is idempotent", async () => {
		const runtime = newRuntime("dashboard_clean");
		const store = createPostgresDashboardStore(runtime, pool);

		await store.ensureInitialized();
		await store.ensureInitialized();

		const expectedVersions = dashboardStoreMigrations(runtime).map(
			(migration) => migration.version,
		);
		const versions = await pool.query<{ version: string }>(
			`SELECT version FROM ${runtime.databaseSchema}.schema_migrations ORDER BY version`,
		);
		expect(versions.rows.map((row) => Number(row.version))).toEqual(
			expectedVersions,
		);

		await store.upsertDevUser("dev-user", "dev@example.test");
		const users = await pool.query<{ id: string; email: string }>(
			`SELECT id, email FROM ${runtime.databaseSchema}.users WHERE id = 'dev-user'`,
		);
		expect(users.rows).toEqual([
			{ id: "dev-user", email: "dev@example.test" },
		]);
	});

});
