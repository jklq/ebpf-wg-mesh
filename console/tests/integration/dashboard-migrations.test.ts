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

	it("upgrades onboarding drafts with the builder column", async () => {
		const runtime = newRuntime("dashboard_upgrade");
		const migrations = dashboardStoreMigrations(runtime);
		const initial = migrations.find((migration) => migration.version === 1);
		if (!initial) {
			throw new Error("expected migration version 1");
		}
		await pool.query(
			`CREATE SCHEMA IF NOT EXISTS ${runtime.databaseSchema}`,
		);
		await pool.query(
			`CREATE TABLE IF NOT EXISTS ${runtime.databaseSchema}.schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`,
		);
		for (const statement of initial.statements) {
			await pool.query(statement);
		}
		await pool.query(
			`INSERT INTO ${runtime.databaseSchema}.schema_migrations (version, applied_at) VALUES (1, NOW())`,
		);
		await pool.query(
			`INSERT INTO ${runtime.databaseSchema}.users (id, email, created_at, updated_at) VALUES ('user-1', 'user@example.test', NOW(), NOW())`,
		);
		await pool.query(
			`INSERT INTO ${runtime.databaseSchema}.onboarding (user_id, repository_selector, tracked_ref, dockerfile_path, context_dir, hostname, created_at, updated_at) VALUES ('user-1', 'octocat/hello', 'main', 'Dockerfile', '.', '', NOW(), NOW())`,
		);

		const store = createPostgresDashboardStore(runtime, pool);
		await store.ensureInitialized();

		const versions = await pool.query<{ version: string }>(
			`SELECT version FROM ${runtime.databaseSchema}.schema_migrations ORDER BY version`,
		);
		expect(versions.rows.map((row) => Number(row.version))).toEqual([1, 2]);

		const draft = await store.getOnboardingDraft("user-1");
		if (!draft) {
			throw new Error("expected onboarding draft");
		}
		expect(draft.builder).toBe("");
		await store.saveOnboardingDraft("user-1", {
			...draft,
			builder: "BUILDER_KIND_RAILPACK",
		});
		const reloaded = await store.getOnboardingDraft("user-1");
		expect(reloaded?.builder).toBe("BUILDER_KIND_RAILPACK");
	});
});
