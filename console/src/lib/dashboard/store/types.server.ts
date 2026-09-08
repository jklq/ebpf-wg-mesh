import type { QueryResult, QueryResultRow } from "pg";

import type { DashboardOnboardingDraft } from "#/lib/dashboard/core/types.server";
import type { GitHubTokenCipher } from "#/lib/dashboard/store/token-crypto.server";

export interface UserRow {
	id: string;
	email: string;
}

export interface GitHubAccountRow {
	user_id: string;
	provider_subject: string;
	verified_email_snapshot: string;
	provider_login: string;
	access_token: string;
	access_token_expires_at: Date | null;
	refresh_token: string;
	refresh_token_expires_at: Date | null;
	token_type: string;
	scope: string;
	oauth_token_version: string | number;
}

export interface OnboardingRow {
	project_id: string;
	environment_id: string;
	service_id: string;
	repository_selector: string;
	tracked_ref: string;
	dockerfile_path: string;
	context_dir: string;
	hostname: string;
}

export interface Queryable {
	query<ResultRow extends QueryResultRow = QueryResultRow>(
		text: string,
		values?: ReadonlyArray<unknown>,
	): Promise<QueryResult<ResultRow>>;
}

export interface DashboardStoreRuntimeConfig {
	databaseSchema: string;
	githubTokenCipher: GitHubTokenCipher;
}

export interface DashboardMigration {
	version: number;
	statements: Array<string>;
}

export function tableName(
	runtime: DashboardStoreRuntimeConfig,
	name: string,
): string {
	return `${runtime.databaseSchema}.${name}`;
}

export function onboardingDraftFromRow(
	row: OnboardingRow,
): DashboardOnboardingDraft {
	return {
		projectId: row.project_id,
		environmentId: row.environment_id,
		serviceId: row.service_id,
		repositorySelector: row.repository_selector,
		trackedRef: row.tracked_ref,
		dockerfilePath: row.dockerfile_path,
		contextDir: row.context_dir,
		hostname: row.hostname,
	};
}
