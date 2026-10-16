import { requireSession } from "#/lib/dashboard/core/auth.server";
import type { DashboardRuntime } from "#/lib/dashboard/core/runtime.server";
import type {
	DashboardProjectUsage,
	DashboardUsageResolution,
} from "#/lib/dashboard/core/types.server";
import { PlatformService } from "#/lib/platform-gen/platform_pb";

export type ProjectUsageInput = {
	projectId: string;
	startTime: string;
	endTime: string;
	resolution: DashboardUsageResolution;
	serviceId?: string;
	resourceBuckets?: boolean;
};

export async function loadProjectUsageFromSession(
	runtime: DashboardRuntime,
	input: ProjectUsageInput,
): Promise<DashboardProjectUsage> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.getProjectUsage,
		session.user,
		{
			projectId: input.projectId,
			startTime: input.startTime,
			endTime: input.endTime,
			resolution: input.resolution,
			serviceId: input.serviceId,
			resourceBuckets: input.resourceBuckets,
		},
	);
}
