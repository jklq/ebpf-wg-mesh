import type { DashboardStoreRuntimeConfig, Queryable } from "./types.server";
import { tableName } from "./types.server";

export const currentDashboardSchemaVersion = 3;
export const minimumDashboardSchemaVersion = 3;

// Legacy schemas have no watermark and therefore admit only their own version.
// Expansion preserves the oldest compatible binary; a later cleanup raises it.
export async function validateDashboardSchema(
	runtime: DashboardStoreRuntimeConfig,
	db: Queryable,
): Promise<void> {
	const metadata = await db.query<{ exists: boolean }>(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='schema_migrations' AND column_name='min_compatible_version') AS exists`,
		[runtime.databaseSchema],
	);
	const compatibility = metadata.rows[0]?.exists
		? "COALESCE(min_compatible_version, version)"
		: "version";
	const result = await db.query<{ version: string; compatible_from: string }>(
		`SELECT version, ${compatibility} AS compatible_from FROM ${tableName(runtime, "schema_migrations")}`,
	);
	const row = result.rows[0];
	const version = Number(row?.version);
	const from = Number(row?.compatible_from);
	if (
		result.rows.length !== 1 ||
		!Number.isSafeInteger(version) ||
		!Number.isSafeInteger(from) ||
		version < minimumDashboardSchemaVersion ||
		from <= 0 ||
		from > version ||
		from > currentDashboardSchemaVersion
	) {
		throw new Error("console schema is incompatible with this release");
	}
}
