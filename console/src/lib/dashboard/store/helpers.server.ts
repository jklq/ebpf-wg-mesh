import { randomUUID } from "node:crypto";
import type { Pool, PoolClient, QueryResult, QueryResultRow } from "pg";
import {
	AuthConflictError,
	type DashboardUser,
	DatabaseError,
	type GitHubAccountLoginInput,
} from "#/lib/dashboard/core/types.server";
import { dashboardStoreMigrations } from "#/lib/dashboard/store/migrations.server";
import type {
	DashboardStoreRuntimeConfig,
	Queryable,
	UserRow,
} from "#/lib/dashboard/store/types.server";
import { tableName } from "#/lib/dashboard/store/types.server";

export async function updateGitHubAccount(
	runtime: DashboardStoreRuntimeConfig,
	client: PoolClient,
	userID: string,
	input: GitHubAccountLoginInput,
): Promise<void> {
	const accessToken = runtime.githubTokenCipher.encrypt(input.accessToken, {
		userID,
		providerSubject: input.providerSubject,
		kind: "access",
	});
	const refreshToken = runtime.githubTokenCipher.encrypt(
		input.refreshToken ?? "",
		{
			userID,
			providerSubject: input.providerSubject,
			kind: "refresh",
		},
	);
	await queryVoid(
		client,
		"updateGitHubAccount",
		`INSERT INTO ${tableName(runtime, "accounts")} (
			id,
			user_id,
			provider,
			provider_subject,
			verified_email_snapshot,
			provider_login,
			access_token,
			access_token_expires_at,
			refresh_token,
			refresh_token_expires_at,
			token_type,
			scope,
			created_at,
			updated_at,
			last_login_at
		) VALUES (
			$1, $2, 'github', $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), NOW(), NOW()
		)
		ON CONFLICT (provider, provider_subject) DO UPDATE
		   SET user_id = excluded.user_id,
		       verified_email_snapshot = excluded.verified_email_snapshot,
		       provider_login = excluded.provider_login,
		       access_token = excluded.access_token,
		       access_token_expires_at = excluded.access_token_expires_at,
		       refresh_token = excluded.refresh_token,
		       refresh_token_expires_at = excluded.refresh_token_expires_at,
		       token_type = excluded.token_type,
		       scope = excluded.scope,
		       oauth_token_version = accounts.oauth_token_version + 1,
		       oauth_refresh_lease_id = NULL,
		       oauth_refresh_lease_expires_at = NULL,
		       updated_at = NOW(),
		       last_login_at = NOW()`,
		[
			randomUUID(),
			userID,
			input.providerSubject,
			input.primaryEmail,
			input.login,
			accessToken,
			input.accessTokenExpiresAt ?? null,
			refreshToken,
			input.refreshTokenExpiresAt ?? null,
			input.tokenType,
			input.scope,
		],
	);
}

export async function userByID(
	runtime: DashboardStoreRuntimeConfig,
	client: PoolClient,
	userID: string,
): Promise<DashboardUser> {
	const result = await query<UserRow>(
		client,
		"userByID",
		`SELECT id, email
		   FROM ${tableName(runtime, "users")}
		  WHERE id = $1`,
		[userID],
	);
	if (result.rowCount !== 1) {
		throw new DatabaseError({
			operation: "userByID",
			message: `user not found: ${userID}`,
			cause: userID,
		});
	}
	return rowAt(result.rows, 0, "userByID");
}

export async function ensureOnboarding(
	runtime: DashboardStoreRuntimeConfig,
	db: Pool | PoolClient,
	userID: string,
): Promise<void> {
	await queryVoid(
		db,
		"ensureOnboarding",
		`INSERT INTO ${tableName(runtime, "onboarding")} (user_id, created_at, updated_at)
		 VALUES ($1, NOW(), NOW())
		 ON CONFLICT (user_id) DO NOTHING`,
		[userID],
	);
}

export async function migrateDashboardStore(
	runtime: DashboardStoreRuntimeConfig,
	db: Pool,
): Promise<void> {
	await queryVoid(
		db,
		"migrate.createSchema",
		`CREATE SCHEMA IF NOT EXISTS ${runtime.databaseSchema}`,
	);

	await withTransaction(db, "migrateDashboardStore", async (client) => {
		await queryVoid(
			client,
			"migrate.createSchemaMigrations",
			`CREATE TABLE IF NOT EXISTS ${tableName(runtime, "schema_migrations")} (
				version INT8 PRIMARY KEY,
				applied_at TIMESTAMPTZ NOT NULL
			)`,
		);
		const result = await query<{ version: string }>(
			client,
			"migrate.selectAppliedVersions",
			`SELECT version FROM ${tableName(runtime, "schema_migrations")}`,
		);
		const appliedVersions = new Set(
			result.rows.map((row) => Number.parseInt(row.version, 10)),
		);
		const currentVersions = new Set(
			dashboardStoreMigrations(runtime).map((migration) => migration.version),
		);
		for (const version of appliedVersions) {
			if (!currentVersions.has(version)) {
				throw new DatabaseError({
					operation: "migrateDashboardStore",
					message: "database schema is stale; recreate the database",
					cause: version,
				});
			}
		}

		for (const migration of dashboardStoreMigrations(runtime)) {
			if (appliedVersions.has(migration.version)) {
				continue;
			}
			for (const statement of migration.statements) {
				await queryVoid(
					client,
					`migrate.statement.${migration.version}`,
					statement,
				);
			}
			await queryVoid(
				client,
				`migrate.recordVersion.${migration.version}`,
				`INSERT INTO ${tableName(runtime, "schema_migrations")} (version, applied_at)
				 VALUES ($1, NOW())`,
				[migration.version],
			);
		}
	});
}

export async function withTransaction<A>(
	db: Pool,
	operation: string,
	program: (client: PoolClient) => Promise<A>,
): Promise<A> {
	const client = await connectClient(db, `${operation}.connect`);
	try {
		await queryVoid(client, `${operation}.begin`, "BEGIN");
		const result = await program(client);
		await queryVoid(client, `${operation}.commit`, "COMMIT");
		return result;
	} catch (cause) {
		try {
			await queryVoid(client, `${operation}.rollback`, "ROLLBACK");
		} catch {
			// Keep the original failure.
		}
		throw cause;
	} finally {
		client.release();
	}
}

async function connectClient(db: Pool, operation: string): Promise<PoolClient> {
	try {
		return await db.connect();
	} catch (cause) {
		throw toDatabaseError(operation, cause);
	}
}

export async function query<Row extends QueryResultRow>(
	db: Queryable,
	operation: string,
	text: string,
	values: ReadonlyArray<unknown> = [],
): Promise<QueryResult<Row>> {
	try {
		return await db.query<Row>(text, values);
	} catch (cause) {
		throw toDatabaseError(operation, cause);
	}
}

export async function queryVoid(
	db: Queryable,
	operation: string,
	text: string,
	values: ReadonlyArray<unknown> = [],
): Promise<void> {
	await query(db, operation, text, values);
}

function toDatabaseError(operation: string, cause: unknown): DatabaseError {
	if (cause instanceof DatabaseError) {
		return cause;
	}
	if (cause instanceof AuthConflictError) {
		throw cause;
	}
	return new DatabaseError({
		operation,
		message: formatCause(cause),
		cause,
	});
}

export function rowAt<Row>(
	rows: Array<Row>,
	index: number,
	operation: string,
): Row {
	const row = rows[index];
	if (row === undefined) {
		throw new DatabaseError({
			operation,
			message: `expected row at index ${index}`,
			cause: rows,
		});
	}
	return row;
}

function formatCause(cause: unknown): string {
	if (cause instanceof AuthConflictError) {
		return cause.message;
	}
	if (cause && typeof cause === "object" && "message" in cause) {
		return String(cause.message);
	}
	return "unknown database error";
}
