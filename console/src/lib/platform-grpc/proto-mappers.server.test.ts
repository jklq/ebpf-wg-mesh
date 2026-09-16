import { create } from "@bufbuild/protobuf";
import { TimestampSchema, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
	AgentLifecycleState,
	BuildStatusSchema,
	DeploymentStatusSchema,
	FleetSchema,
	ServiceLogLineSchema,
	ServiceLogType,
} from "#/lib/platform-gen/platform_pb";
import {
	toBuildStatus,
	toCreateAgentRequest,
	toDeploymentStatus,
	toFleet,
	toServiceLogLine,
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

	it("keeps missing build and deployment timestamps absent", () => {
		const build = toBuildStatus(
			create(BuildStatusSchema, { buildId: "build-1" }),
		);
		expect(build?.queuedAt).toBeUndefined();
		expect(build?.startedAt).toBeUndefined();
		expect(build?.finishedAt).toBeUndefined();

		const status = toDeploymentStatus(
			create(DeploymentStatusSchema, { deploymentId: "deploy-1" }),
		);
		expect(status?.transitionedAt).toBeUndefined();
	});

	it("keeps zero and epoch timestamps absent", () => {
		const epoch = create(TimestampSchema, { seconds: 0n, nanos: 0 });
		const build = toBuildStatus(
			create(BuildStatusSchema, {
				buildId: "build-1",
				queuedAt: epoch,
				startedAt: epoch,
				finishedAt: epoch,
			}),
		);
		expect(build?.queuedAt).toBeUndefined();
		expect(build?.startedAt).toBeUndefined();
		expect(build?.finishedAt).toBeUndefined();

		const status = toDeploymentStatus(
			create(DeploymentStatusSchema, {
				deploymentId: "deploy-1",
				transitionedAt: epoch,
			}),
		);
		expect(status?.transitionedAt).toBeUndefined();
	});

	it("keeps malformed timestamps absent", () => {
		const build = toBuildStatus(
			create(BuildStatusSchema, {
				buildId: "build-1",
				queuedAt: create(TimestampSchema, {
					seconds: 100n,
					nanos: 2_000_000_000,
				}),
			}),
		);
		expect(build?.queuedAt).toBeUndefined();
	});

	it("maps valid build and deployment timestamps", () => {
		const queuedAt = new Date("2026-08-13T10:00:00Z");
		const build = toBuildStatus(
			create(BuildStatusSchema, {
				buildId: "build-1",
				queuedAt: timestampFromDate(queuedAt),
			}),
		);
		expect(build?.queuedAt).toEqual(queuedAt);
	});

	it("keeps missing log timestamps absent", () => {
		const line = toServiceLogLine(
			create(ServiceLogLineSchema, { line: "booting", sequence: 1n }),
		);
		expect(line.observedAt).toBeUndefined();
	});
});
