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
			version: 3,
			statements: [
				`CREATE TABLE ${tableName(runtime, "users")} (
					id TEXT PRIMARY KEY,
					email TEXT NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE TABLE ${tableName(runtime, "refresh_sessions")} (
					id TEXT PRIMARY KEY,
					user_id TEXT NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					created_at TIMESTAMPTZ NOT NULL,
					expires_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE INDEX ${runtime.databaseSchema}_refresh_sessions_expires_at_idx
					ON ${tableName(runtime, "refresh_sessions")} (expires_at)`,
				`CREATE TABLE ${tableName(runtime, "accounts")} (
					id TEXT PRIMARY KEY,
					user_id TEXT NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					provider TEXT NOT NULL,
					provider_subject TEXT NOT NULL,
					verified_email_snapshot TEXT NOT NULL DEFAULT '',
					provider_login TEXT NOT NULL DEFAULT '',
					access_token TEXT NOT NULL DEFAULT '',
					access_token_expires_at TIMESTAMPTZ NULL,
					refresh_token TEXT NOT NULL DEFAULT '',
					refresh_token_expires_at TIMESTAMPTZ NULL,
					token_type TEXT NOT NULL DEFAULT '',
					scope TEXT NOT NULL DEFAULT '',
					oauth_token_version INT8 NOT NULL DEFAULT 0,
					oauth_refresh_lease_id TEXT NULL,
					oauth_refresh_lease_expires_at TIMESTAMPTZ NULL,
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL,
					last_login_at TIMESTAMPTZ NOT NULL,
					UNIQUE(provider, provider_subject)
				)`,
				`CREATE INDEX ${runtime.databaseSchema}_accounts_provider_email_idx
					ON ${tableName(runtime, "accounts")} (provider, lower(verified_email_snapshot))`,
				`CREATE TABLE ${tableName(runtime, "onboarding")} (
					user_id TEXT PRIMARY KEY REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					project_id TEXT NOT NULL DEFAULT '',
					environment_id TEXT NOT NULL DEFAULT '',
					service_id TEXT NOT NULL DEFAULT '',
					repository_selector TEXT NOT NULL DEFAULT '',
					tracked_ref TEXT NOT NULL DEFAULT '',
					dockerfile_path TEXT NOT NULL DEFAULT '',
					context_dir TEXT NOT NULL DEFAULT '',
					builder TEXT NOT NULL DEFAULT '',
					hostname TEXT NOT NULL DEFAULT '',
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE TABLE ${tableName(runtime, "service_positions")} (
					user_id TEXT NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					environment_id TEXT NOT NULL,
					service_id TEXT NOT NULL,
					x INT8 NOT NULL,
					y INT8 NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (user_id, environment_id, service_id)
				)`,
				`CREATE INDEX ${runtime.databaseSchema}_service_positions_environment_idx
					ON ${tableName(runtime, "service_positions")} (user_id, environment_id)`,
			],
		},
	];
}
