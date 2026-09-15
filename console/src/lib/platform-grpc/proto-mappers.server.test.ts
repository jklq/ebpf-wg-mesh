import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
	AgentLifecycleState,
	EnvironmentSchema,
	FleetSchema,
	ServiceLogLineSchema,
	ServiceLogType,
	ServiceSchema,
} from "#/lib/platform-gen/platform_pb";
import {
	toCreateAgentRequest,
	toEnvironment,
	toFleet,
	toServiceLogLine,
	toServiceRecord,
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

	it("maps environment auto-deploy into dashboard values", () => {
		const environment = toEnvironment(
			create(EnvironmentSchema, {
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: 1,
				isProduction: true,
				autoDeploy: false,
			}),
		);
		expect(environment).toMatchObject({
			id: "environment-1",
			isProduction: true,
			autoDeploy: false,
		});
	});

	it("maps the latest source revision into the service summary", () => {
		const observedAt = new Date("2026-09-15T12:00:00Z");
		const service = toServiceRecord(
			create(ServiceSchema, {
				id: "service-1",
				environmentId: "environment-1",
				name: "web",
				sourceSummary: {
					source: {
						case: "sourceState",
						value: {
							latestRevision: {
								commitSha: "bbb222bbb222",
								observedAt: timestampFromDate(observedAt),
							},
						},
					},
				},
			}),
		);
		expect(service.sourceSummary?.latestRevision).toEqual({
			commitSha: "bbb222bbb222",
			observedAt,
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
});
