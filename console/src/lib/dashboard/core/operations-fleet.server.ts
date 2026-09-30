import { requireSession } from "#/lib/dashboard/core/auth.server";
import type { DashboardRuntime } from "#/lib/dashboard/core/runtime.server";
import type {
	DashboardAgentEnrollment,
	DashboardAgentLifecycleState,
	DashboardFleet,
	DashboardFleetAgent,
	FleetAgentInput,
} from "#/lib/dashboard/core/types.server";
import { OpsService } from "#/lib/platform-gen/platform_pb";
import { normalizeFleetAgentInput } from "./operations-helpers.server";

export async function loadFleetFromSession(
	runtime: DashboardRuntime,
): Promise<DashboardFleet> {
	const session = await requireSession(runtime);
	return runtime.platform.call(OpsService.method.listFleet, session.user, {});
}

export async function createFleetAgentFromSession(
	runtime: DashboardRuntime,
	input: FleetAgentInput,
): Promise<DashboardAgentEnrollment> {
	const session = await requireSession(runtime);
	return runtime.platform.call(OpsService.method.createAgent, session.user, {
		...normalizeFleetAgentInput(input),
		reservedCpuMillis: String(input.reservedCpuMillis),
		reservedMemoryMebibytes: String(input.reservedMemoryMebibytes),
	});
}

export async function updateFleetAgentFromSession(
	runtime: DashboardRuntime,
	input: FleetAgentInput,
): Promise<DashboardFleetAgent> {
	const session = await requireSession(runtime);
	return runtime.platform.call(OpsService.method.updateAgent, session.user, {
		...normalizeFleetAgentInput(input),
		reservedCpuMillis: String(input.reservedCpuMillis),
		reservedMemoryMebibytes: String(input.reservedMemoryMebibytes),
	});
}

export async function setFleetAgentLifecycleFromSession(
	runtime: DashboardRuntime,
	input: { agentId: string; lifecycleState: DashboardAgentLifecycleState },
): Promise<DashboardFleetAgent> {
	const session = await requireSession(runtime);
	return runtime.platform.call(
		OpsService.method.setAgentLifecycle,
		session.user,
		{
			agentId: input.agentId.trim(),
			lifecycleState: input.lifecycleState,
		},
	);
}
