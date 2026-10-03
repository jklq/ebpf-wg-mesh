import { requireSession } from "#/lib/dashboard/core/auth.server";
import type { DashboardRuntime } from "#/lib/dashboard/core/runtime.server";
import type {
	DashboardUser,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { PlatformService } from "#/lib/platform-gen/platform_pb";

export async function createVolumeFromSession(
	runtime: DashboardRuntime,
	input: { environmentId: string; name: string; sizeBytes: number },
): Promise<DashboardVolume> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.createVolume,
		session.user,
		{
			environmentId: input.environmentId,
			name: input.name.trim(),
			sizeBytes: String(input.sizeBytes),
		},
	);
}

export async function growVolumeFromSession(
	runtime: DashboardRuntime,
	input: { volumeId: string; sizeBytes: number },
): Promise<DashboardVolume> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		PlatformService.method.updateVolume,
		session.user,
		{
			volumeId: input.volumeId,
			sizeBytes: String(input.sizeBytes),
		},
	);
}

export async function listEnvironmentVolumes(
	runtime: DashboardRuntime,
	user: DashboardUser,
	environmentId: string,
): Promise<Array<DashboardVolume>> {
	return runtime.platform
		.call(PlatformService.method.listVolumes, user, { environmentId })
		.then((response) => response.volumes ?? []);
}
