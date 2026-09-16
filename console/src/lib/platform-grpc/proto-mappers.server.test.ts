import { create } from "@bufbuild/protobuf";
import { TimestampSchema, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
	AgentLifecycleState,
	BuilderKind,
	BuildRecipeSchema,
	BuildStatusSchema,
	DeploymentStatusSchema,
	EnvironmentSchema,
	FleetSchema,
	RestartCause,
	ServiceLogLineSchema,
	ServiceLogType,
	ServiceSchema,
	ServiceStatusSchema,
} from "#/lib/platform-gen/platform_pb";
import {
	toBuildStatus,
	toCreateAgentRequest,
	toDeploymentStatus,
	toEnvironment,
	toFleet,
	toProtoServiceSpec,
	toRepositoryInspection,
	toServiceLogLine,
	toServiceRecord,
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
