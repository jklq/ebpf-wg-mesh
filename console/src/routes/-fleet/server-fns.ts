import { createServerFn } from "@tanstack/react-start";
import { z } from "zod";

const identifier = z.string().min(1);
const fleetAgentInput = z.object({
	agentId: identifier,
	name: identifier,
	region: z.string(),
	zone: z.string(),
	failureDomain: z.string(),
	reservedCpuMillis: z.number().int().nonnegative(),
	reservedMemoryMebibytes: z.number().int().nonnegative(),
});
const agentLifecycleState = z.enum([
	"AGENT_LIFECYCLE_STATE_ENROLLING",
	"AGENT_LIFECYCLE_STATE_ACTIVE",
	"AGENT_LIFECYCLE_STATE_CORDONED",
	"AGENT_LIFECYCLE_STATE_DRAINING",
	"AGENT_LIFECYCLE_STATE_UNAVAILABLE",
	"AGENT_LIFECYCLE_STATE_RETIRED",
	"AGENT_LIFECYCLE_STATE_UNSPECIFIED",
]);

export const loadFleet = createServerFn({ method: "GET" }).handler(async () =>
	(await import("#/lib/dashboard/entry.server")).loadFleetFromSession(),
);

export const doCreateFleetAgent = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => fleetAgentInput.parse(input))
	.handler(async ({ data }) =>
		(await import("#/lib/dashboard/entry.server")).createFleetAgentFromSession(
			data,
		),
	);

export const doUpdateFleetAgent = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => fleetAgentInput.parse(input))
	.handler(async ({ data }) =>
		(await import("#/lib/dashboard/entry.server")).updateFleetAgentFromSession(
			data,
		),
	);

export const doSetFleetAgentLifecycle = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) =>
		z
			.object({ agentId: identifier, lifecycleState: agentLifecycleState })
			.parse(input),
	)
	.handler(async ({ data }) =>
		(
			await import("#/lib/dashboard/entry.server")
		).setFleetAgentLifecycleFromSession(data),
	);
