import { randomUUID } from "node:crypto";
import type { Pool, PoolClient, QueryResult, QueryResultRow } from "pg";

import {
	AuthConflictError,
	DatabaseError,
	type DashboardOnboardingDraft,
	type DashboardStore,
	type DashboardUser,
	type GitHubAccountLoginInput,
} from "#/lib/dashboard-core.server";

interface UserRow {
	id: string;
	subject: string;
	email: string;
}

interface GitHubAccountRow {
	provider_subject: string;
	verified_email_snapshot: string;
	provider_login: string;
	access_token: string;
	access_token_expires_at: Date | null;
	refresh_token: string;
	refresh_token_expires_at: Date | null;
	token_type: string;
	scope: string;
}

interface OnboardingRow {
	current_step: string;
	project_id: string;
	service_id: string;
	repository_selector: string;
	tracked_ref: string;
	dockerfile_path: string;
	context_dir: string;
	container_port: string;
	hostname: string;
}

interface Queryable {
	query<ResultRow extends QueryResultRow = QueryResultRow>(
		text: string,
		values?: ReadonlyArray<unknown>,
	): Promise<QueryResult<ResultRow>>;
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

	function ensureInitialized(): Promise<void> {
		if (!initPromise) {
			initPromise = migrateDashboardStore(runtime, db);
		}
		return initPromise;
	}

	return {
		ensureInitialized,

		async upsertDevUser(subject, email) {
			const id = randomUUID();
			const result = await query<UserRow>(
				db,
				"upsertDevUser.user",
				`INSERT INTO ${tableName(runtime, "users")} (id, subject, email, created_at, updated_at)
				 VALUES ($1, $2, $3, NOW(), NOW())
				 ON CONFLICT (subject) DO UPDATE SET email = excluded.email, updated_at = NOW()
				 RETURNING id, subject, email`,
				[id, subject, email],
			);
			const user = rowAt(result.rows, 0, "upsertDevUser.user");
			await queryVoid(
				db,
				"upsertDevUser.account",
				`INSERT INTO ${tableName(runtime, "accounts")} (
					id, user_id, provider, provider_subject, verified_email_snapshot, provider_login, created_at, updated_at, last_login_at
				) VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW(), NOW())
				ON CONFLICT (provider, provider_subject) DO UPDATE
				   SET verified_email_snapshot = excluded.verified_email_snapshot,
				       provider_login = excluded.provider_login,
				       updated_at = NOW(),
				       last_login_at = NOW()`,
				[randomUUID(), user.id, "dev", user.subject, user.email, user.subject],
			);
			await ensureOnboarding(runtime, db, user.id);
			return user;
		},

		async completeGitHubLogin(input) {
			return withTransaction(db, "completeGitHubLogin", async (client) => {
				const existingAccount = await query<{ user_id: string }>(
					client,
					"completeGitHubLogin.existingAccount",
					`SELECT user_id
					   FROM ${tableName(runtime, "accounts")}
					  WHERE provider = 'github' AND provider_subject = $1`,
					[input.providerSubject],
				);
				if ((existingAccount.rowCount ?? 0) === 1) {
					const userID = rowAt(
						existingAccount.rows,
						0,
						"completeGitHubLogin.existingAccount",
					).user_id;
					await updateGitHubAccount(runtime, client, userID, input);
					const user = await userByID(runtime, client, userID);
					return { user, disposition: "login" as const };
				}

				const exactEmailUsers = await query<UserRow>(
					client,
					"completeGitHubLogin.exactEmailUsers",
					`SELECT id, subject, email
					   FROM ${tableName(runtime, "users")}
					  WHERE lower(email) = lower($1)
					  ORDER BY id ASC`,
					[input.primaryEmail],
				);
				if ((exactEmailUsers.rowCount ?? 0) > 1) {
					throw new AuthConflictError({
						code: "ambiguous_existing_user",
						message: "multiple users match the verified email",
					});
				}

				const conflictingGitHubEmail = await query<{ user_id: string }>(
					client,
					"completeGitHubLogin.conflictingGitHubEmail",
					`SELECT user_id
					   FROM ${tableName(runtime, "accounts")}
					  WHERE provider = 'github'
					    AND lower(COALESCE(verified_email_snapshot, '')) = lower($1)
					    AND provider_subject <> $2
					  LIMIT 1`,
					[input.primaryEmail, input.providerSubject],
				);
				if ((conflictingGitHubEmail.rowCount ?? 0) > 0) {
					throw new AuthConflictError({
						code: "email_linked_to_other_github",
						message:
							"verified email is already linked to another GitHub account",
					});
				}

				if ((exactEmailUsers.rowCount ?? 0) === 1) {
					const user = rowAt(
						exactEmailUsers.rows,
						0,
						"completeGitHubLogin.exactEmailUsers",
					);
					const existingGitHubAccount = await query<{
						provider_subject: string;
					}>(
						client,
						"completeGitHubLogin.existingGitHubAccount",
						`SELECT provider_subject
						   FROM ${tableName(runtime, "accounts")}
						  WHERE user_id = $1 AND provider = 'github'`,
						[user.id],
					);
					if ((existingGitHubAccount.rowCount ?? 0) > 0) {
						throw new AuthConflictError({
							code: "email_linked_to_other_github",
							message:
								"existing user is already linked to another GitHub account",
						});
					}
					await queryVoid(
						client,
						"completeGitHubLogin.updateUserEmail",
						`UPDATE ${tableName(runtime, "users")}
						    SET email = $2, updated_at = NOW()
						  WHERE id = $1`,
						[user.id, input.primaryEmail],
					);
					await updateGitHubAccount(runtime, client, user.id, input);
					await ensureOnboarding(runtime, client, user.id);
					return {
						user: { ...user, email: input.primaryEmail },
						disposition: "link" as const,
					};
				}

				const userID = randomUUID();
				const subject = `user_${userID.replace(/-/g, "")}`;
				const created = await query<UserRow>(
					client,
					"completeGitHubLogin.createUser",
					`INSERT INTO ${tableName(runtime, "users")} (id, subject, email, created_at, updated_at)
					 VALUES ($1, $2, $3, NOW(), NOW())
					 RETURNING id, subject, email`,
					[userID, subject, input.primaryEmail],
				);
				const user = rowAt(created.rows, 0, "completeGitHubLogin.createUser");
				await updateGitHubAccount(runtime, client, user.id, input);
				await ensureOnboarding(runtime, client, user.id);
				return { user, disposition: "signup" as const };
			});
		},

		async getGitHubAccount(userID) {
			const result = await query<GitHubAccountRow>(
				db,
				"getGitHubAccount",
				`SELECT provider_subject,
				        verified_email_snapshot,
				        provider_login,
				        access_token,
				        access_token_expires_at,
				        refresh_token,
				        refresh_token_expires_at,
				        token_type,
				        scope
				   FROM ${tableName(runtime, "accounts")}
				  WHERE user_id = $1 AND provider = 'github'`,
				[userID],
			);
			if (result.rowCount !== 1) {
				return null;
			}
			const row = rowAt(result.rows, 0, "getGitHubAccount");
			return {
				providerSubject: row.provider_subject,
				login: row.provider_login,
				primaryEmail: row.verified_email_snapshot,
				accessToken: row.access_token,
				tokenType: row.token_type,
				scope: row.scope,
				accessTokenExpiresAt: row.access_token_expires_at ?? undefined,
				refreshToken: row.refresh_token || undefined,
				refreshTokenExpiresAt: row.refresh_token_expires_at ?? undefined,
			};
		},

		async getOnboardingDraft(userID) {
			await ensureOnboarding(runtime, db, userID);
			const result = await query<OnboardingRow>(
				db,
				"getOnboardingDraft",
				`SELECT current_step,
				        project_id,
				        service_id,
				        repository_selector,
				        tracked_ref,
				        dockerfile_path,
				        context_dir,
				        container_port,
				        hostname
				   FROM ${tableName(runtime, "onboarding")}
				  WHERE user_id = $1`,
				[userID],
			);
			return onboardingDraftFromRow(
				rowAt(result.rows, 0, "getOnboardingDraft"),
			);
		},

		async saveOnboardingDraft(userID, draft) {
			await ensureOnboarding(runtime, db, userID);
			const result = await query<OnboardingRow>(
				db,
				"saveOnboardingDraft",
				`UPDATE ${tableName(runtime, "onboarding")}
				    SET current_step = $2,
				        project_id = $3,
				        service_id = $4,
				        repository_selector = $5,
				        tracked_ref = $6,
				        dockerfile_path = $7,
				        context_dir = $8,
				        container_port = $9,
				        hostname = $10,
				        updated_at = NOW()
				  WHERE user_id = $1
				RETURNING current_step,
				          project_id,
				          service_id,
				          repository_selector,
				          tracked_ref,
				          dockerfile_path,
				          context_dir,
				          container_port,
				          hostname`,
				[
					userID,
					draft.currentStep,
					draft.projectId,
					draft.serviceId,
					draft.repositorySelector,
					draft.trackedRef,
					draft.dockerfilePath,
					draft.contextDir,
					draft.containerPort,
					draft.hostname,
				],
			);
			return onboardingDraftFromRow(
				rowAt(result.rows, 0, "saveOnboardingDraft"),
			);
		},

		async createRefreshSession(sessionId, userID, expiresAt) {
			await queryVoid(
				db,
				"createRefreshSession",
				`INSERT INTO ${tableName(runtime, "refresh_sessions")} (id, user_id, created_at, expires_at)
				 VALUES ($1, $2, NOW(), $3)`,
				[sessionId, userID, expiresAt],
			);
		},

		async deleteRefreshSession(sessionId) {
			await queryVoid(
				db,
				"deleteRefreshSession",
				`DELETE FROM ${tableName(runtime, "refresh_sessions")} WHERE id = $1`,
				[sessionId],
			);
		},

		async rotateRefreshSession(input) {
			return withTransaction(db, "rotateRefreshSession", async (client) => {
				const result = await query<UserRow>(
					client,
					"rotateRefreshSession.select",
					`SELECT u.id, u.subject, u.email
					   FROM ${tableName(runtime, "refresh_sessions")} rs
					   JOIN ${tableName(runtime, "users")} u ON u.id = rs.user_id
					  WHERE rs.id = $1
					    AND rs.user_id = $2
					    AND rs.expires_at > $3`,
					[input.sessionId, input.userID, input.now],
				);
				if (result.rowCount !== 1) {
					return null;
				}
				const user = rowAt(result.rows, 0, "rotateRefreshSession.select");
				await queryVoid(
					client,
					"rotateRefreshSession.deleteCurrent",
					`DELETE FROM ${tableName(runtime, "refresh_sessions")} WHERE id = $1`,
					[input.sessionId],
				);
				await queryVoid(
					client,
					"rotateRefreshSession.insertNext",
					`INSERT INTO ${tableName(runtime, "refresh_sessions")} (id, user_id, created_at, expires_at)
					 VALUES ($1, $2, NOW(), $3)`,
					[input.nextSessionId, input.userID, input.expiresAt],
				);
				return user;
			});
		},
	};
}

async function updateGitHubAccount(
	runtime: DashboardStoreRuntimeConfig,
	client: PoolClient,
	userID: string,
	input: GitHubAccountLoginInput,
): Promise<void> {
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
		       updated_at = NOW(),
		       last_login_at = NOW()`,
		[
			randomUUID(),
			userID,
			input.providerSubject,
			input.primaryEmail,
			input.login,
			input.accessToken,
			input.accessTokenExpiresAt ?? null,
			input.refreshToken ?? "",
			input.refreshTokenExpiresAt ?? null,
			input.tokenType,
			input.scope,
		],
	);
}

async function userByID(
	runtime: DashboardStoreRuntimeConfig,
	client: PoolClient,
	userID: string,
): Promise<DashboardUser> {
	const result = await query<UserRow>(
		client,
		"userByID",
		`SELECT id, subject, email
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

async function ensureOnboarding(
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

async function migrateDashboardStore(
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

async function withTransaction<A>(
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

async function query<Row extends QueryResultRow>(
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

async function queryVoid(
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

function rowAt<Row>(rows: Array<Row>, index: number, operation: string): Row {
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
		{
			version: 2,
			statements: [
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS verified_email_snapshot STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS provider_login STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS access_token STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS access_token_expires_at TIMESTAMPTZ NULL`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS refresh_token STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS refresh_token_expires_at TIMESTAMPTZ NULL`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS token_type STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS scope STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NULL`,
				`UPDATE ${tableName(runtime, "accounts")} SET updated_at = created_at WHERE updated_at IS NULL`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ALTER COLUMN updated_at SET NOT NULL`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMPTZ NULL`,
				`UPDATE ${tableName(runtime, "accounts")} SET last_login_at = created_at WHERE last_login_at IS NULL`,
				`ALTER TABLE ${tableName(runtime, "accounts")} ALTER COLUMN last_login_at SET NOT NULL`,
				`CREATE INDEX IF NOT EXISTS ${runtime.databaseSchema}_accounts_provider_email_idx
					ON ${tableName(runtime, "accounts")} (provider, lower(verified_email_snapshot))`,
			],
		},
		{
			version: 3,
			statements: [
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS current_step STRING NOT NULL DEFAULT 'account'`,
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS project_id STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS service_id STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS repository_selector STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS tracked_ref STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS dockerfile_path STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS context_dir STRING NOT NULL DEFAULT ''`,
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS hostname STRING NOT NULL DEFAULT ''`,
			],
		},
		{
			version: 4,
			statements: [
				`ALTER TABLE ${tableName(runtime, "onboarding")} ADD COLUMN IF NOT EXISTS container_port STRING NOT NULL DEFAULT ''`,
			],
		},
		{
			version: 5,
			statements: [
				`CREATE TABLE IF NOT EXISTS ${tableName(runtime, "refresh_sessions")} (
						id STRING PRIMARY KEY,
						user_id STRING NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
						created_at TIMESTAMPTZ NOT NULL,
						expires_at TIMESTAMPTZ NOT NULL
					)`,
				`CREATE INDEX IF NOT EXISTS ${runtime.databaseSchema}_refresh_sessions_expires_at_idx
						ON ${tableName(runtime, "refresh_sessions")} (expires_at)`,
			],
		},
	];
}

function tableName(runtime: DashboardStoreRuntimeConfig, name: string): string {
	return `${runtime.databaseSchema}.${name}`;
}

function onboardingDraftFromRow(row: OnboardingRow): DashboardOnboardingDraft {
	return {
		currentStep: decodeOnboardingStep(row.current_step),
		projectId: row.project_id,
		serviceId: row.service_id,
		repositorySelector: row.repository_selector,
		trackedRef: row.tracked_ref,
		dockerfilePath: row.dockerfile_path,
		contextDir: row.context_dir,
		containerPort: row.container_port,
		hostname: row.hostname,
	};
}

function decodeOnboardingStep(
	value: string,
): DashboardOnboardingDraft["currentStep"] {
	switch (value) {
		case "repository":
		case "build":
		case "domain":
			return value;
		default:
			return "account";
	}
}
