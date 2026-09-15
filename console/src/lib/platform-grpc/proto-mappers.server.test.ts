import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
	AgentLifecycleState,
	BuilderKind,
	BuildRecipeSchema,
	BuildStatusSchema,
	FleetSchema,
	ServiceLogLineSchema,
	ServiceLogType,
} from "#/lib/platform-gen/platform_pb";
import {
	toBuildStatus,
	toCreateAgentRequest,
	toFleet,
	toProtoServiceSpec,
	toRepositoryInspection,
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

	it("maps inspection analysis and both recipe recommendations", () => {
		const inspection = toRepositoryInspection({
			accessState: 2,
			defaultBranch: "main",
			dockerfileCandidates: ["Dockerfile"],
			recommendedBuildRecipe: create(BuildRecipeSchema, {
				builder: BuilderKind.RAILPACK,
				contextDir: "apps/web",
			}),
			recommendedDockerfileRecipe: create(BuildRecipeSchema, {
				builder: BuilderKind.DOCKERFILE,
				dockerfilePath: "Dockerfile",
				contextDir: ".",
			}),
			recommendedPorts: [3000],
			detectedLanguage: "node",
			detectedStartCommand: "npm run start",
			analysisError: "",
		});

		expect(inspection.detectedLanguage).toBe("node");
		expect(inspection.detectedStartCommand).toBe("npm run start");
		expect(inspection.analysisError).toBe("");
		expect(inspection.recommendedBuildRecipe).toMatchObject({
			builder: "BUILDER_KIND_RAILPACK",
			contextDir: "apps/web",
		});
		expect(inspection.recommendedDockerfileRecipe).toMatchObject({
			builder: "BUILDER_KIND_DOCKERFILE",
			dockerfilePath: "Dockerfile",
		});
	});

	it("maps failed analysis without a recommended recipe", () => {
		const inspection = toRepositoryInspection({
			accessState: 2,
			defaultBranch: "main",
			dockerfileCandidates: ["Dockerfile"],
			recommendedPorts: [],
			detectedLanguage: "node",
			detectedStartCommand: "",
			analysisError: "no start command was found",
		});

		expect(inspection.recommendedBuildRecipe).toBeUndefined();
		expect(inspection.analysisError).toBe("no start command was found");
	});

	it("maps the builder on build status and service specs", () => {
		const status = toBuildStatus(
			create(BuildStatusSchema, {
				buildId: "build-1",
				builder: BuilderKind.RAILPACK,
			}),
		);
		if (!status) {
			throw new Error("expected build status");
		}
		expect(status.builder).toBe("BUILDER_KIND_RAILPACK");

		const legacy = toBuildStatus(
			create(BuildStatusSchema, { buildId: "build-2" }),
		);
		if (!legacy) {
			throw new Error("expected build status");
		}
		expect(legacy.builder).toBeUndefined();

		const spec = toProtoServiceSpec({
			source: {
				provider: "github",
				repositorySelector: "octocat/hello",
				trackedRef: "main",
				buildRecipe: {
					builder: "BUILDER_KIND_DOCKERFILE",
					dockerfilePath: "Dockerfile",
					contextDir: ".",
				},
			},
			runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		});
		const sourceSpec = spec.source?.source;
		if (sourceSpec?.case !== "sourceSpec") {
			throw new Error("expected source spec");
		}
		expect(sourceSpec.value.buildRecipe?.builder).toBe(BuilderKind.DOCKERFILE);
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
