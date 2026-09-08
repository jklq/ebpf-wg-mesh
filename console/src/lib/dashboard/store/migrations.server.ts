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
				`CREATE TABLE ${tableName(runtime, "users")} (
					id STRING PRIMARY KEY,
					email STRING NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE TABLE ${tableName(runtime, "refresh_sessions")} (
					id STRING PRIMARY KEY,
					user_id STRING NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					created_at TIMESTAMPTZ NOT NULL,
					expires_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE INDEX ${runtime.databaseSchema}_refresh_sessions_expires_at_idx
					ON ${tableName(runtime, "refresh_sessions")} (expires_at)`,
				`CREATE TABLE ${tableName(runtime, "accounts")} (
					id STRING PRIMARY KEY,
					user_id STRING NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					provider STRING NOT NULL,
					provider_subject STRING NOT NULL,
					verified_email_snapshot STRING NOT NULL DEFAULT '',
					provider_login STRING NOT NULL DEFAULT '',
					access_token STRING NOT NULL DEFAULT '',
					access_token_expires_at TIMESTAMPTZ NULL,
					refresh_token STRING NOT NULL DEFAULT '',
					refresh_token_expires_at TIMESTAMPTZ NULL,
					token_type STRING NOT NULL DEFAULT '',
					scope STRING NOT NULL DEFAULT '',
					oauth_token_version INT8 NOT NULL DEFAULT 0,
					oauth_refresh_lease_id STRING NULL,
					oauth_refresh_lease_expires_at TIMESTAMPTZ NULL,
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL,
					last_login_at TIMESTAMPTZ NOT NULL,
					UNIQUE(provider, provider_subject)
				)`,
				`CREATE INDEX ${runtime.databaseSchema}_accounts_provider_email_idx
					ON ${tableName(runtime, "accounts")} (provider, lower(verified_email_snapshot))`,
				`CREATE TABLE ${tableName(runtime, "onboarding")} (
					user_id STRING PRIMARY KEY REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					project_id STRING NOT NULL DEFAULT '',
					environment_id STRING NOT NULL DEFAULT '',
					service_id STRING NOT NULL DEFAULT '',
					repository_selector STRING NOT NULL DEFAULT '',
					tracked_ref STRING NOT NULL DEFAULT '',
					dockerfile_path STRING NOT NULL DEFAULT '',
					context_dir STRING NOT NULL DEFAULT '',
					hostname STRING NOT NULL DEFAULT '',
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL
				)`,
				`CREATE TABLE ${tableName(runtime, "service_positions")} (
					user_id STRING NOT NULL REFERENCES ${tableName(runtime, "users")}(id) ON DELETE CASCADE,
					environment_id STRING NOT NULL,
					service_id STRING NOT NULL,
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
