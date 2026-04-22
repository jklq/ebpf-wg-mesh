import type {
	DashboardMigration,
	DashboardStoreRuntimeConfig,
} from "#/lib/dashboard/store/types.server";
import { tableName } from "#/lib/dashboard/store/types.server";

export function dashboardStoreMigrations(
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
