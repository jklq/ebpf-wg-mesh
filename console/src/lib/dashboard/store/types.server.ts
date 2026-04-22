import type { QueryResult, QueryResultRow } from "pg";

import type { DashboardOnboardingDraft } from "#/lib/dashboard/core/types.server";

export interface UserRow {
	id: string;
	subject: string;
	email: string;
}

export interface GitHubAccountRow {
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

export interface OnboardingRow {
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

export interface Queryable {
	query<ResultRow extends QueryResultRow = QueryResultRow>(
		text: string,
		values?: ReadonlyArray<unknown>,
	): Promise<QueryResult<ResultRow>>;
}

export interface DashboardStoreRuntimeConfig {
	databaseSchema: string;
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
