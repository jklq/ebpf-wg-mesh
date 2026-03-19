import { randomUUID } from "node:crypto";
import type { Pool } from "pg";

import type {
	DashboardStore,
	DashboardUser,
} from "#/lib/dashboard-core.server";

interface DashboardSessionRecord {
	sessionId: string;
	user: DashboardUser;
}

export interface DashboardStoreRuntimeConfig {
	databaseSchema: string;
}

interface DashboardMigration {
	version: number;
	statements: Array<string>;
}

export function createPostgresDashboardStore(
	runtime: DashboardStoreRuntimeConfig,
	db: Pool,
): DashboardStore {
	let initPromise: Promise<void> | undefined;

	async function ensureInitialized(): Promise<void> {
		if (!initPromise) {
			initPromise = migrateDashboardStore(runtime, db);
		}
		await initPromise;
	}

	return {
		ensureInitialized,
		async upsertUser(subject, email): Promise<DashboardUser> {
			const id = randomUUID();
			const result = await db.query<{
				id: string;
				subject: string;
				email: string;
			}>(
				`INSERT INTO ${tableName(runtime, "users")} (id, subject, email, created_at, updated_at)
				 VALUES ($1, $2, $3, NOW(), NOW())
				 ON CONFLICT (subject) DO UPDATE SET email = excluded.email, updated_at = NOW()
				 RETURNING id, subject, email`,
				[id, subject, email],
			);
			const user = result.rows[0];
			await db.query(
				`INSERT INTO ${tableName(runtime, "accounts")} (id, user_id, provider, provider_subject, created_at)
				 VALUES ($1, $2, $3, $4, NOW())
				 ON CONFLICT (provider, provider_subject) DO NOTHING`,
				[randomUUID(), user.id, "dev", user.subject],
			);
			await db.query(
				`INSERT INTO ${tableName(runtime, "onboarding")} (user_id, created_at, updated_at)
				 VALUES ($1, NOW(), NOW())
				 ON CONFLICT (user_id) DO NOTHING`,
				[user.id],
			);
			return user;
		},
		async createSession(sessionId, userID, expiresAt): Promise<void> {
			await db.query(
				`INSERT INTO ${tableName(runtime, "sessions")} (id, user_id, created_at, expires_at)
				 VALUES ($1, $2, NOW(), $3)`,
				[sessionId, userID, expiresAt],
			);
		},
		async deleteSession(sessionId): Promise<void> {
			await db.query(
				`DELETE FROM ${tableName(runtime, "sessions")} WHERE id = $1`,
				[sessionId],
			);
		},
		async getSession(sessionId, now): Promise<DashboardSessionRecord | null> {
			const result = await db.query<{
				user_id: string;
				subject: string;
				email: string;
			}>(
				`SELECT s.user_id, u.subject, u.email
				   FROM ${tableName(runtime, "sessions")} s
				   JOIN ${tableName(runtime, "users")} u ON u.id = s.user_id
				  WHERE s.id = $1 AND s.expires_at > $2`,
				[sessionId, now],
			);
			if (result.rowCount !== 1) {
				return null;
			}
			const row = result.rows[0];
			return {
				sessionId,
				user: {
					id: row.user_id,
					subject: row.subject,
					email: row.email,
				},
			};
		},
	};
}

async function migrateDashboardStore(
	runtime: DashboardStoreRuntimeConfig,
	db: Pool,
): Promise<void> {
	await db.query(`CREATE SCHEMA IF NOT EXISTS ${runtime.databaseSchema}`);

	const client = await db.connect();
	try {
		await client.query("BEGIN");
		await client.query(
			`CREATE TABLE IF NOT EXISTS ${tableName(runtime, "schema_migrations")} (
				version INT8 PRIMARY KEY,
				applied_at TIMESTAMPTZ NOT NULL
			)`,
		);
		const result = await client.query<{ version: string }>(
			`SELECT version FROM ${tableName(runtime, "schema_migrations")}`,
		);
		const appliedVersions = new Set(
			result.rows.map((row) => Number.parseInt(row.version, 10)),
		);
		for (const migration of dashboardStoreMigrations(runtime)) {
			if (appliedVersions.has(migration.version)) {
				continue;
			}
			for (const statement of migration.statements) {
				await client.query(statement);
			}
			await client.query(
				`INSERT INTO ${tableName(runtime, "schema_migrations")} (version, applied_at)
				 VALUES ($1, NOW())`,
				[migration.version],
			);
		}
		await client.query("COMMIT");
	} catch (error) {
		await client.query("ROLLBACK");
		throw error;
	} finally {
		client.release();
	}
}

function dashboardStoreMigrations(
	runtime: DashboardStoreRuntimeConfig,
): Array<DashboardMigration> {
	return [
		{
			version: 1,
			statements: [
				`CREATE TABLE IF NOT EXISTS ${tableName(runtime, "users")} (
					id STRING PRIMARY KEY,
					subject STRING NOT NULL UNIQUE,
					email STRING NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE TABLE IF NOT EXISTS ${tableName(runtime, "sessions")} (
					id STRING PRIMARY KEY,
					user_id STRING NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					created_at TIMESTAMPTZ NOT NULL,
					expires_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE TABLE IF NOT EXISTS ${tableName(runtime, "accounts")} (
					id STRING PRIMARY KEY,
					user_id STRING NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					provider STRING NOT NULL,
					provider_subject STRING NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					UNIQUE(provider, provider_subject)
				)`,
				`CREATE TABLE IF NOT EXISTS ${tableName(runtime, "onboarding")} (
					user_id STRING PRIMARY KEY REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					account_name STRING NOT NULL DEFAULT '',
					status STRING NOT NULL DEFAULT 'pending',
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS ${runtime.databaseSchema}_sessions_expires_at_idx
					ON ${tableName(runtime, "sessions")} (expires_at)`,
			],
		},
	];
}

function tableName(runtime: DashboardStoreRuntimeConfig, name: string): string {
	return `${runtime.databaseSchema}.${name}`;
}
