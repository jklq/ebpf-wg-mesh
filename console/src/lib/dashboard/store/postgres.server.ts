import { randomUUID } from "node:crypto";
import type { Pool } from "pg";

import {
	AuthConflictError,
	type DashboardStore,
} from "#/lib/dashboard/core/types.server";
import {
	ensureOnboarding,
	migrateDashboardStore,
	query,
	queryVoid,
	rowAt,
	updateGitHubAccount,
	userByID,
	withTransaction,
} from "#/lib/dashboard/store/helpers.server";
import type {
	DashboardStoreRuntimeConfig,
	GitHubAccountRow,
	OnboardingRow,
	UserRow,
} from "#/lib/dashboard/store/types.server";
import {
	onboardingDraftFromRow,
	tableName,
} from "#/lib/dashboard/store/types.server";

export type { DashboardStoreRuntimeConfig } from "#/lib/dashboard/store/types.server";

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

		async ensureSessionUser(user) {
			await queryVoid(
				db,
				"ensureSessionUser",
				`INSERT INTO ${tableName(runtime, "users")} (
					id, subject, email, created_at, updated_at
				) VALUES ($1, $2, $3, NOW(), NOW())
				ON CONFLICT (id) DO UPDATE SET
				   subject = excluded.subject,
				   email = excluded.email,
				   updated_at = NOW()`,
				[user.id, user.subject, user.email],
			);
		},

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
				        hostname = $9,
				        updated_at = NOW()
				  WHERE user_id = $1
				RETURNING current_step,
				          project_id,
				          service_id,
				          repository_selector,
				          tracked_ref,
				          dockerfile_path,
				          context_dir,
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
