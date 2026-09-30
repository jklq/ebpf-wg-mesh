import {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "#/lib/dashboard/core/defaults";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { ServiceSchema } from "#/lib/platform-gen/platform_pb";
import {
	fromPlatformJson,
	integerString,
	toPlatformJson,
} from "#/lib/platform-json";

export interface PendingServiceCreation {
	clientId: string;
	selector: string;
	name: string;
	position: { x: number; y: number };
}
export function pendingServiceRecord(
	entry: PendingServiceCreation,
	environmentId: string | null,
): DashboardServiceRecord {
	const id = `pending-${entry.clientId}`;
	const now = new Date().toISOString();
	return toPlatformJson(
		ServiceSchema,
		fromPlatformJson(ServiceSchema, {
			id,
			environmentId: environmentId ?? "",
			name: entry.name,
			spec: {
				source: {
					sourceSpec: {
						provider: "github",
						repositorySelector: entry.selector,
						trackedRef: "main",
						buildRecipe: {
							builder: "BUILDER_KIND_RAILPACK",
							dockerfilePath: "",
							contextDir: ".",
						},
					},
				},
				runtime: {
					env: {},
					cpuMillis: integerString(DEFAULT_SERVICE_CPU_MILLIS),
					memoryMebibytes: integerString(DEFAULT_SERVICE_MEMORY_MEBIBYTES),
					ports: [],
				},
			},
			createdAt: now,
			updatedAt: now,
		}),
	);
}
