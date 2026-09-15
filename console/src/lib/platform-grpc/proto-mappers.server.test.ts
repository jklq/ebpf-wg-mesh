import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
	AgentLifecycleState,
	FleetSchema,
	RestartCause,
	ServiceLogLineSchema,
	ServiceLogType,
	ServiceStatusSchema,
} from "#/lib/platform-gen/platform_pb";
import {
	toCreateAgentRequest,
	toFleet,
	toServiceLogLine,
	toServiceStatus,
} from "#/lib/platform-grpc/proto-mappers.server";

describe("platform protobuf mappers", () => {
	it("maps generated fleet messages into dashboard values", () => {
		const fleet = toFleet(
			create(FleetSchema, {
				agents: [
					{
						id: "node-a",
						name: "edge-a",
						lifecycleState: AgentLifecycleState.CORDONED,
						schedulableCpuMillis: 1500n,
						allocationCount: 2,
					},
				],
				capacity: { schedulableNodeCount: 2, headroomCpuMillis: 800n },
				versionWarning: "fleet software version skew detected",
			}),
		);

		expect(fleet.agents[0]).toMatchObject({
			id: "node-a",
			lifecycleState: "AGENT_LIFECYCLE_STATE_CORDONED",
			schedulableCpuMillis: 1500,
			allocationCount: 2,
		});
		expect(fleet.capacity.schedulableNodeCount).toBe(2);
	});

	it("maps protobuf timestamps and symbolic enum names", () => {
		const observedAt = new Date("2026-04-24T11:00:00Z");
		const line = toServiceLogLine(
			create(ServiceLogLineSchema, {
				observedAt: timestampFromDate(observedAt),
				line: "ready",
				logType: ServiceLogType.RUNTIME,
				sequence: 42n,
			}),
		);
		expect(line).toMatchObject({
			observedAt,
			line: "ready",
			logType: "SERVICE_LOG_TYPE_RUNTIME",
			sequence: 42,
		});
	});

	it("creates valid generated request messages", () => {
		const request = toCreateAgentRequest({
			agentId: "node-a",
			name: "edge-a",
			region: "eu-west",
			zone: "eu-west-1",
			failureDomain: "rack-a",
			reservedCpuMillis: 250,
			reservedMemoryMebibytes: 512,
		});
		expect(request.$typeName).toBe("platform.v1.CreateAgentRequest");
		expect(request.reservedCpuMillis).toBe(250n);
	});

	it("maps full crash evidence from allocation restart observations", () => {
		const lastRestartAt = new Date("2026-04-24T11:00:00Z");
		const status = toServiceStatus(
			create(ServiceStatusSchema, {
				service: {
					id: "svc-1",
					environmentId: "env-1",
					name: "web",
				},
				allocations: [
					{
						allocationId: "alloc-1",
						serviceId: "svc-1",
						agentId: "agent-1",
						phase: "CrashLoop",
						restart: {
							restartCount: 5,
							crashLoop: true,
							lastCause: RestartCause.OOM_KILL,
							message: "crash loop after OOM kill",
							lastExitCode: 137,
							lastSignal: 9,
							awaitingRestart: false,
							lastRestartAt: timestampFromDate(lastRestartAt),
						},
					},
				],
			}),
		);
		expect(status.allocations?.[0].restart).toMatchObject({
			restartCount: 5,
			crashLoop: true,
			lastCause: "RESTART_CAUSE_OOM_KILL",
			message: "crash loop after OOM kill",
			lastExitCode: 137,
			lastSignal: 9,
			awaitingRestart: false,
			lastRestartAt,
		});
	});
});
