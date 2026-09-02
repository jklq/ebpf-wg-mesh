import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	platformCall,
} from "#/lib/dashboard/core/runtime.server";
import type {
	DashboardAgentEnrollment,
	DashboardAgentLifecycleState,
	DashboardFleet,
	DashboardFleetAgent,
	FleetAgentInput,
} from "#/lib/dashboard/core/types.server";
import { normalizeFleetAgentInput } from "./operations-helpers.server";

export async function loadFleetFromSession(
	runtime: DashboardRuntime,
): Promise<DashboardFleet> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "listFleet", (platform) =>
		platform.listFleet(session.user),
	);
}

export async function createFleetAgentFromSession(
	runtime: DashboardRuntime,
	input: FleetAgentInput,
): Promise<DashboardAgentEnrollment> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "createFleetAgent", (platform) =>
		platform.createFleetAgent(session.user, normalizeFleetAgentInput(input)),
	);
}

export async function updateFleetAgentFromSession(
	runtime: DashboardRuntime,
	input: FleetAgentInput,
): Promise<DashboardFleetAgent> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "updateFleetAgent", (platform) =>
		platform.updateFleetAgent(session.user, normalizeFleetAgentInput(input)),
	);
}

export async function setFleetAgentLifecycleFromSession(
	runtime: DashboardRuntime,
	input: { agentId: string; lifecycleState: DashboardAgentLifecycleState },
): Promise<DashboardFleetAgent> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "setFleetAgentLifecycle", (platform) =>
		platform.setFleetAgentLifecycle(session.user, {
			agentId: input.agentId.trim(),
			lifecycleState: input.lifecycleState,
		}),
	);
}
