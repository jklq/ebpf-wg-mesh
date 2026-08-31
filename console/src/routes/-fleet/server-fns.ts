import { createServerFn } from "@tanstack/react-start";

import type {
	DashboardAgentLifecycleState,
	FleetAgentInput,
} from "#/lib/dashboard/core/types.server";

export const loadFleet = createServerFn({ method: "GET" }).handler(async () =>
	(await import("#/lib/dashboard/entry.server")).loadFleetFromSession(),
);

export const doCreateFleetAgent = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => input as FleetAgentInput)
	.handler(async ({ data }) =>
		(
			await import("#/lib/dashboard/entry.server")
		).createFleetAgentFromSession(data),
	);

export const doUpdateFleetAgent = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => input as FleetAgentInput)
	.handler(async ({ data }) =>
		(
			await import("#/lib/dashboard/entry.server")
		).updateFleetAgentFromSession(data),
	);

export const doSetFleetAgentLifecycle = createServerFn({ method: "POST" })
	.inputValidator(
		(input: unknown) =>
			input as {
				agentId: string;
				lifecycleState: DashboardAgentLifecycleState;
			},
	)
	.handler(async ({ data }) =>
		(
			await import("#/lib/dashboard/entry.server")
		).setFleetAgentLifecycleFromSession(data),
	);
