import { describe, expect, it } from "vitest";
import * as auth from "#/lib/dashboard/core/auth.server";
import { createSessionTokenPair } from "#/lib/dashboard/core/jwt.server";
import * as homeOperations from "#/lib/dashboard/core/operations-home.server";
import * as onboarding from "#/lib/dashboard/core/operations-onboarding.server";
import * as services from "#/lib/dashboard/core/operations-services.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";
import { jsonFixture, serviceFixture } from "#/lib/dashboard/testkit/protocol";
import {
	EnvironmentSchema,
	InspectSourceResponseSchema,
	ProjectSchema,
	ServiceLogLineSchema,
	ServiceRestartSchema,
	ServiceRuntimeSchema,
	ServiceSpecSchema,
} from "#/lib/platform-gen/platform_pb";

describe("dashboard operations", () => {
	it("returns degraded home state when the control plane is unavailable", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.errors.listProjects = new Error("control plane down");

		const state = await homeOperations.loadDashboardHome(harness.runtime);

		expect(state).not.toBeNull();
		expect(state?.controlPlaneReachable).toBe(false);
		expect(state?.controlPlaneError).toContain("control plane down");
	});

	it("does not expose fleet management when the user lacks operator access", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.errors.listFleet = new Error("operator access required");

		const state = await homeOperations.loadDashboardHome(harness.runtime);

		expect(state?.controlPlaneReachable).toBe(true);
		expect(state?.canManageFleet).toBe(false);
	});

	it("exposes fleet management when the operator probe succeeds", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		const state = await homeOperations.loadDashboardHome(harness.runtime);

		expect(state?.canManageFleet).toBe(true);
	});

	it("includes saved service positions in home state", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.projects = [
			jsonFixture(ProjectSchema, {
				id: "project-1",
				name: "project",
				kind: "PROJECT_KIND_USER",
			}),
		];
		harness.platform.services = [
			serviceFixture({
				id: "service-1",
				environmentId: "environment-project-1",
				name: "hello",
				spec: {
					runtime: {
						env: {},
						cpuMillis: "250",
						memoryMebibytes: "256",
						ports: [],
					},
				},
			}),
		];
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-project-1",
			serviceId: "service-1",
			repositorySelector: "",
			trackedRef: "",
			builder: "BUILDER_KIND_RAILPACK",
			dockerfilePath: "",
			contextDir: ".",
			hostname: "",
		});

		await services.saveServicePositionFromSession(harness.runtime, {
			environmentId: "environment-project-1",
			serviceId: "service-1",
			position: { x: 320, y: 256 },
		});
		const state = await homeOperations.loadDashboardHome(harness.runtime);

		expect(state?.services[0]?.layoutPosition).toEqual({ x: 320, y: 256 });
		expect(state?.selectedServiceId).toBe("service-1");
	});

	it("uses the URL environment to select its project and services", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.projects = [
			jsonFixture(ProjectSchema, {
				id: "project-1",
				name: "one",
				kind: "PROJECT_KIND_USER",
			}),
			jsonFixture(ProjectSchema, {
				id: "project-2",
				name: "two",
				kind: "PROJECT_KIND_USER",
			}),
		];
		harness.platform.environments = [
			jsonFixture(EnvironmentSchema, {
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "ENVIRONMENT_KIND_PERSISTENT",
				isProduction: true,
				autoDeploy: false,
			}),
			jsonFixture(EnvironmentSchema, {
				id: "environment-2",
				projectId: "project-2",
				name: "Staging",
				kind: "ENVIRONMENT_KIND_PERSISTENT",
				isProduction: false,
				autoDeploy: true,
			}),
		];
		harness.platform.services = [
			serviceFixture({
				id: "service-1",
				environmentId: "environment-1",
				name: "one-service",
				spec: {
					runtime: {
						env: {},
						cpuMillis: "250",
						memoryMebibytes: "256",
						ports: [],
					},
				},
			}),
			serviceFixture({
				id: "service-2",
				environmentId: "environment-2",
				name: "two-service",
				spec: {
					runtime: {
						env: {},
						cpuMillis: "250",
						memoryMebibytes: "256",
						ports: [],
					},
				},
			}),
		];
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "service-1",
			repositorySelector: "",
			trackedRef: "",
			builder: "BUILDER_KIND_RAILPACK",
			dockerfilePath: "",
			contextDir: ".",
			hostname: "",
		});

		const state = await homeOperations.loadDashboardHome(
			harness.runtime,
			"environment-2",
		);

		expect(state?.project?.id).toBe("project-2");
		expect(state?.environment?.id).toBe("environment-2");
		expect(state?.services.map((service) => service.id)).toEqual(["service-2"]);
		expect(state?.onboarding).toMatchObject({
			projectId: "project-2",
			environmentId: "environment-2",
			serviceId: "",
		});
	});

	it("creates projects from the active session", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		const project = await onboarding.createProjectFromSession(
			harness.runtime,
			"demo-app",
		);

		expect(project.name).toBe("demo-app");
		expect(harness.platform.calls.createProject).toEqual([
			{
				user: {
					id: "user-1",
					email: "user@example.com",
				},
				name: "demo-app",
			},
		]);
	});

	it("does not list GitHub repositories on initial home load", async () => {
		const harness = createDashboardTestHarness();
		await auth.beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		await auth.completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});

		const home = await homeOperations.loadDashboardHome(harness.runtime);

		expect(home?.githubAccount?.login).toBe("octocat");
		expect(home?.repositories).toEqual([]);
		expect(harness.github.listRepositoriesCalls).toEqual([]);
	});

	it("returns project and services without loading status or domain bindings", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.projects = [
			jsonFixture(ProjectSchema, {
				id: "project-1",
				name: "project",
				kind: "PROJECT_KIND_USER",
			}),
		];
		harness.platform.services = [
			serviceFixture({
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					runtime: {
						env: {},
						cpuMillis: "250",
						memoryMebibytes: "256",
						ports: [],
					},
				},
			}),
		];
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-project-1",
			serviceId: "service-1",
			repositorySelector: "",
			trackedRef: "",
			builder: "BUILDER_KIND_RAILPACK",
			dockerfilePath: "",
			contextDir: ".",
			hostname: "hello.example.test",
		});

		const home = await homeOperations.loadDashboardHome(harness.runtime);

		expect(home?.project?.id).toBe("project-1");
		expect(home?.services).toHaveLength(1);
		expect(home?.selectedServiceId).toBe("service-1");
		expect(BigInt(home?.servicesRevision ?? "0")).toBeGreaterThanOrEqual(1n);
		expect(home?.domainBindings).toEqual([]);
		expect(harness.platform.calls.getServiceStatus).toEqual([]);
		expect(harness.platform.calls.listDomainBindings).toEqual([]);
	});

	it("initializes the dashboard schema before reading a home page from an existing access token", async () => {
		const harness = createDashboardTestHarness();
		const session = createSessionTokenPair(
			harness.config,
			{
				id: "user-1",
				email: "user@example.com",
			},
			"session-1",
			new Date("2026-03-18T12:00:00Z"),
		);
		await harness.store.upsertDevUser("user-1", "user@example.com");
		harness.cookies.values.set(
			harness.config.sessionCookieName,
			session.accessToken,
		);

		const home = await homeOperations.loadDashboardHome(harness.runtime);

		expect(home).not.toBeNull();
		expect(harness.storeEnsureInitializedCalls).toHaveLength(1);
	});

	it("creates another service when deploying the same repository again", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.projects = [
			jsonFixture(ProjectSchema, {
				id: "project-1",
				name: "brisk-harbor",
				kind: "PROJECT_KIND_USER",
			}),
		];
		harness.platform.services = [
			serviceFixture({
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					source: {
						sourceSpec: {
							provider: "github",
							repositorySelector: "octocat/hello",
							trackedRef: "main",
							buildRecipe: {
								builder: "BUILDER_KIND_DOCKERFILE",
								dockerfilePath: "Dockerfile",
								contextDir: ".",
							},
						},
					},
					runtime: {
						env: {},
						cpuMillis: "250",
						memoryMebibytes: "256",
						ports: [{ port: 8080, primary: true }],
					},
				},
			}),
		];
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-project-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			builder: "BUILDER_KIND_DOCKERFILE",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		});

		const result = await onboarding.createServiceFastFromSession(
			harness.runtime,
			{
				repositorySelector: "octocat/hello",
				serviceName: "talented-harmony",
			},
		);
		const draft = result.onboarding;

		expect(harness.platform.calls.updateService).toHaveLength(0);
		expect(harness.platform.calls.createService).toMatchObject([
			{
				user: {
					id: "user-1",
					email: "user@example.com",
				},
				environmentId: "environment-project-1",
				service: {
					name: "talented-harmony",
					spec: jsonFixture(ServiceSpecSchema, {
						source: {
							sourceSpec: {
								provider: "github",
								repositorySelector: "octocat/hello",
								trackedRef: "main",
								buildRecipe: {
									builder: "BUILDER_KIND_RAILPACK",
									dockerfilePath: "",
									contextDir: ".",
								},
							},
						},
						desiredReplicaCount: 1,
						runtime: jsonFixture(ServiceRuntimeSchema, {
							env: {},
							cpuMillis: "250",
							memoryMebibytes: "256",
							ports: [],
						}),
					}),
				},
			},
		]);
		expect(draft.serviceId).toBe("service-2");
		expect(draft.builder).toBe("BUILDER_KIND_RAILPACK");
	});

	it("seeds service runtime ports from repository inspection", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.nextRepositoryInspection = jsonFixture(
			InspectSourceResponseSchema,
			{
				accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
				defaultBranch: "main",
				dockerfileCandidates: ["Dockerfile"],
				recommendedBuildRecipe: {
					builder: "BUILDER_KIND_RAILPACK",
					dockerfilePath: "",
					contextDir: ".",
				},
				recommendedPorts: [3000, 8080],
				detectedLanguage: "node",
				detectedStartCommand: "npm run start",
				analysisError: "",
			},
		);

		await onboarding.createServiceFastFromSession(harness.runtime, {
			repositorySelector: "octocat/hello",
			serviceName: "talented-harmony",
		});

		expect(
			harness.platform.calls.createService[0].service?.spec?.runtime?.ports,
		).toEqual([
			{ port: 3000, primary: true },
			{ port: 8080, primary: false },
		]);
	});

	it("creates dockerfile services when explicitly selected", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		const result = await onboarding.createServiceFastFromSession(
			harness.runtime,
			{
				repositorySelector: "octocat/hello",
				serviceName: "talented-harmony",
				builder: "BUILDER_KIND_DOCKERFILE",
				dockerfilePath: "deploy/Dockerfile",
				contextDir: "deploy",
			},
		);

		expect(
			harness.platform.calls.createService[0].service?.spec?.source?.sourceSpec,
		).toMatchObject({
			buildRecipe: {
				builder: "BUILDER_KIND_DOCKERFILE",
				dockerfilePath: "deploy/Dockerfile",
				contextDir: "deploy",
			},
		});
		expect(result.onboarding.builder).toBe("BUILDER_KIND_DOCKERFILE");
	});

	it("falls back to dockerfile when railpack analysis fails", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.nextRepositoryInspection = jsonFixture(
			InspectSourceResponseSchema,
			{
				accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
				defaultBranch: "main",
				dockerfileCandidates: ["Dockerfile"],
				recommendedDockerfileRecipe: {
					builder: "BUILDER_KIND_DOCKERFILE",
					dockerfilePath: "Dockerfile",
					contextDir: ".",
				},
				recommendedPorts: [],
				detectedLanguage: "node",
				detectedStartCommand: "",
				analysisError: 'detected Node.js in "." but no start command was found',
			},
		);

		const result = await onboarding.createServiceFastFromSession(
			harness.runtime,
			{
				repositorySelector: "octocat/hello",
			},
		);

		expect(
			harness.platform.calls.createService[0].service?.spec?.source?.sourceSpec,
		).toMatchObject({
			buildRecipe: {
				builder: "BUILDER_KIND_DOCKERFILE",
				dockerfilePath: "Dockerfile",
				contextDir: ".",
			},
		});
		expect(result.onboarding.builder).toBe("BUILDER_KIND_DOCKERFILE");
	});

	it("refuses an explicit railpack create when analysis fails", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.nextRepositoryInspection = jsonFixture(
			InspectSourceResponseSchema,
			{
				accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
				defaultBranch: "main",
				dockerfileCandidates: ["Dockerfile"],
				recommendedDockerfileRecipe: {
					builder: "BUILDER_KIND_DOCKERFILE",
					dockerfilePath: "Dockerfile",
					contextDir: ".",
				},
				recommendedPorts: [],
				detectedLanguage: "node",
				detectedStartCommand: "",
				analysisError: 'detected Node.js in "." but no start command was found',
			},
		);

		await expect(
			onboarding.createServiceFastFromSession(harness.runtime, {
				repositorySelector: "octocat/hello",
				builder: "BUILDER_KIND_RAILPACK",
			}),
		).rejects.toThrow("no start command was found");
		expect(harness.platform.calls.createService).toEqual([]);
	});

	it("switches the builder through settings updates", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.services = [
			serviceFixture({
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					source: {
						sourceSpec: {
							provider: "github",
							repositorySelector: "octocat/hello",
							trackedRef: "main",
							buildRecipe: {
								builder: "BUILDER_KIND_RAILPACK",
								dockerfilePath: "",
								contextDir: ".",
							},
						},
					},
					runtime: {
						env: {},
						cpuMillis: "250",
						memoryMebibytes: "256",
						ports: [],
					},
				},
			}),
		];

		await services.updateServiceFromSession(harness.runtime, {
			serviceId: "service-1",
			builder: "BUILDER_KIND_DOCKERFILE",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
		});

		expect(
			harness.platform.calls.updateService[0].service?.spec?.source?.sourceSpec,
		).toMatchObject({
			buildRecipe: {
				builder: "BUILDER_KIND_DOCKERFILE",
				dockerfilePath: "Dockerfile",
				contextDir: ".",
			},
		});
	});

	it("returns fast-created service details for immediate rendering", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});

		const result = await onboarding.createServiceFastFromSession(
			harness.runtime,
			{
				repositorySelector: "octocat/hello",
				serviceName: "talented-harmony",
			},
		);

		expect(result.project.id).toBe("project-1");
		expect(result.service).toMatchObject({
			id: "service-1",
			environmentId: "environment-1",
			name: "talented-harmony",
		});
		expect(result.serviceStatus?.service?.id).toBe("service-1");
		expect(result.onboarding).toMatchObject({
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
		});
		expect(
			harness.platform.calls.createService[0].service?.spec?.runtime,
		).toMatchObject({
			cpuMillis: "250",
			memoryMebibytes: "256",
		});
	});

	it("rejects repositories the signed-in GitHub account cannot access", async () => {
		const harness = createDashboardTestHarness({ devUsers: [] });
		await auth.beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		await auth.completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});

		await expect(
			onboarding.createServiceFastFromSession(harness.runtime, {
				repositorySelector: "someone/private-repository",
			}),
		).rejects.toThrow(
			"The signed-in GitHub account cannot access this repository.",
		);
		expect(harness.platform.calls.createProject).toEqual([]);
		expect(harness.platform.calls.linkGitHubRepository).toEqual([]);
		expect(harness.platform.calls.createService).toEqual([]);
	});

	it("forwards rich service log filters to the platform", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.serviceLogs = [
			jsonFixture(ServiceLogLineSchema, {
				allocationId: "",
				agentId: "",
				stream: "deploy",
				rolloutGeneration: "1",
				sequence: "1",
				line: "initializing service",
				logType: "SERVICE_LOG_TYPE_DEPLOY",
				buildId: "build-1",
				stage: "initialization",
			}),
		];

		const logs = await services.listServiceLogsFromSession(harness.runtime, {
			serviceId: "service-1",
			limit: 500,
			logType: "SERVICE_LOG_TYPE_DEPLOY",
			buildId: "build-1",
			search: "initializing",
		});

		expect(logs.lines).toHaveLength(1);
		expect(harness.platform.calls.listServiceLogs[0]).toMatchObject({
			serviceId: "service-1",
			limit: 500,
			logType: "SERVICE_LOG_TYPE_DEPLOY",
			buildId: "build-1",
			search: "initializing",
		});
	});

	it("updates a service display name with settings changes", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.services = [
			serviceFixture({
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "old-name",
				spec: {
					source: {
						sourceSpec: {
							provider: "github",
							repositorySelector: "octocat/hello",
							trackedRef: "main",
							buildRecipe: {
								builder: "BUILDER_KIND_DOCKERFILE",
								dockerfilePath: "Dockerfile",
								contextDir: ".",
							},
						},
					},
					runtime: {
						env: {},
						cpuMillis: "250",
						memoryMebibytes: "256",
						ports: [{ port: 8080, primary: true }],
						healthCheck: { path: "/ready", port: 8080, timeoutSeconds: 3 },
						volume: { volumeName: "data", mountPath: "/data" },
					},
				},
			}),
		];
		harness.platform.environments = [
			jsonFixture(EnvironmentSchema, {
				id: "environment-project-1",
				projectId: "project-1",
				name: "Production",
				kind: "ENVIRONMENT_KIND_PERSISTENT",
				isProduction: true,
				autoDeploy: false,
			}),
		];

		const updated = await services.updateServiceFromSession(harness.runtime, {
			serviceId: "service-1",
			serviceName: "talented-harmony",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			builder: "BUILDER_KIND_DOCKERFILE",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
		});

		expect(updated.name).toBe("talented-harmony");
		expect(harness.platform.calls.updateService[0]).toMatchObject({
			serviceId: "service-1",
			service: {
				name: "talented-harmony",
				spec: {
					source: {
						sourceSpec: {
							buildRecipe: {
								builder: "BUILDER_KIND_DOCKERFILE",
								dockerfilePath: "Dockerfile",
								contextDir: ".",
							},
						},
					},
					runtime: {
						healthCheck: {
							path: "/ready",
							port: 8080,
							timeoutSeconds: 3,
						},
						volume: { volumeName: "data", mountPath: "/data" },
					},
				},
			},
		});
	});

	it("preserves a direct image and unedited runtime fields when changing its name", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		const current = serviceFixture({
			id: "service-1",
			environmentId: "environment-1",
			name: "before",
			spec: {
				source: { image: { image: "registry.example/app@sha256:abc" } },
				desiredReplicaCount: 2,
				runtime: {
					command: ["/app/server"],
					args: ["--debug"],
					env: { MODE: "production" },
					cpuMillis: "9007199254740993",
					memoryMebibytes: "512",
					ports: [{ port: 8080, primary: true }],
					volume: { volumeName: "data", mountPath: "/data" },
					healthCheck: { path: "/ready", port: 8080, timeoutSeconds: 3 },
					livenessCheck: { path: "/live", port: 8080, timeoutSeconds: 5 },
				},
			},
		});
		harness.platform.services = [current];

		const updated = await services.updateServiceFromSession(harness.runtime, {
			serviceId: current.id,
			serviceName: "after",
		});

		expect(updated.name).toBe("after");
		expect(updated.spec).toEqual(current.spec);
		expect(harness.platform.calls.updateService[0].service?.spec).toEqual(
			current.spec,
		);
		expect(harness.platform.calls.linkGitHubRepository).toEqual([]);
	});

	it("merges restart settings without resetting custom backoff fields", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		const current = serviceFixture({
			id: "service-1",
			spec: {
				runtime: {
					restart: {
						policy: "RESTART_POLICY_ON_FAILURE",
						maxRestarts: 4,
						windowSeconds: 120,
						initialDelayMs: 250,
						maxDelayMs: 5000,
						backoffMultiplier: 1.5,
						jitter: 0.2,
						stableAfterSeconds: 30,
					},
				},
			},
		});
		harness.platform.services = [current];

		const updated = await services.updateServiceFromSession(harness.runtime, {
			serviceId: current.id,
			restart: { policy: "RESTART_POLICY_NEVER" },
		});

		expect(updated.spec?.runtime?.restart).toEqual({
			...current.spec?.runtime?.restart,
			policy: "RESTART_POLICY_NEVER",
		});
	});

	it("does not reload or relink GitHub for settings updates on the same repository", async () => {
		const harness = createDashboardTestHarness({ devUsers: [] });
		await auth.beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		await auth.completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});
		harness.platform.services = [repositoryBackedService()];

		await services.updateServiceFromSession(harness.runtime, {
			serviceId: "service-1",
			repositorySelector: " OctoCat / Hello ",
			restart: jsonFixture(ServiceRestartSchema, {
				policy: "RESTART_POLICY_NEVER",
				maxRestarts: 5,
				windowSeconds: 300,
			}),
		});

		expect(harness.github.listRepositoriesCalls).toEqual([]);
		expect(harness.platform.calls.linkGitHubRepository).toEqual([]);
		expect(harness.platform.calls.updateService).toHaveLength(1);
	});

	it("validates and links GitHub when the repository changes", async () => {
		const harness = createDashboardTestHarness({ devUsers: [] });
		harness.github.nextRepositories.push({
			owner: "octocat",
			name: "other",
			fullName: "octocat/other",
			private: false,
			defaultBranch: "main",
		});
		await auth.beginGitHubLogin(harness.runtime, { redirectTo: "/" });
		await auth.completeAuthCallback(harness.runtime, {
			code: "github-code",
			state: "session-1",
		});
		harness.platform.services = [repositoryBackedService()];
		harness.platform.environments = [
			jsonFixture(EnvironmentSchema, {
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "ENVIRONMENT_KIND_PERSISTENT",
				isProduction: true,
				autoDeploy: false,
			}),
		];

		await services.updateServiceFromSession(harness.runtime, {
			serviceId: "service-1",
			repositorySelector: "octocat/other",
		});

		expect(harness.github.listRepositoriesCalls).toEqual([
			"github-access-token",
		]);
		expect(harness.platform.calls.linkGitHubRepository).toMatchObject([
			{
				projectId: "project-1",
				repositorySelector: "octocat/other",
				githubUserAccessToken: "github-access-token",
			},
		]);
		expect(harness.platform.calls.updateService).toHaveLength(1);
	});

	it("deletes a service through the platform", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.services = [
			serviceFixture({
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "doomed",
			}),
		];

		await services.deleteServiceFromSession(harness.runtime, {
			serviceId: "service-1",
		});

		expect(harness.platform.calls.deleteService).toMatchObject([
			{ serviceId: "service-1" },
		]);
		expect(harness.platform.services).toEqual([]);
	});
});

function repositoryBackedService() {
	return serviceFixture({
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "hello",
		spec: {
			source: {
				sourceSpec: {
					provider: "github" as const,
					repositorySelector: "octocat/hello",
					trackedRef: "main",
					buildRecipe: { dockerfilePath: "Dockerfile", contextDir: "." },
				},
			},
			runtime: {
				env: {},
				cpuMillis: "250",
				memoryMebibytes: "256",
				ports: [],
			},
		},
	});
}
