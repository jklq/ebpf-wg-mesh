import { describe, expect, it } from "vitest";
import * as auth from "#/lib/dashboard/core/auth.server";
import { createSessionTokenPair } from "#/lib/dashboard/core/jwt.server";
import * as homeOperations from "#/lib/dashboard/core/operations-home.server";
import * as onboarding from "#/lib/dashboard/core/operations-onboarding.server";
import * as services from "#/lib/dashboard/core/operations-services.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";

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
		harness.platform.listFleet = async () => {
			throw new Error("operator access required");
		};

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
			{ id: "project-1", name: "project", kind: "PROJECT_KIND_USER" },
		];
		harness.platform.services = [
			{
				id: "service-1",
				environmentId: "environment-project-1",
				name: "hello",
				spec: {
					runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
				},
			},
		];
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-project-1",
			serviceId: "service-1",
			repositorySelector: "",
			trackedRef: "",
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
		expect(state?.service?.layoutPosition).toEqual({ x: 320, y: 256 });
	});

	it("uses the URL environment to select its project and services", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.projects = [
			{ id: "project-1", name: "one", kind: "PROJECT_KIND_USER" },
			{ id: "project-2", name: "two", kind: "PROJECT_KIND_USER" },
		];
		harness.platform.environments = [
			{
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "persistent",
				isProduction: true,
			},
			{
				id: "environment-2",
				projectId: "project-2",
				name: "Staging",
				kind: "persistent",
				isProduction: false,
			},
		];
		harness.platform.services = [
			{
				id: "service-1",
				environmentId: "environment-1",
				name: "one-service",
				spec: {
					runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
				},
			},
			{
				id: "service-2",
				environmentId: "environment-2",
				name: "two-service",
				spec: {
					runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
				},
			},
		];
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "service-1",
			repositorySelector: "",
			trackedRef: "",
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
		expect(harness.platform.createProjectCalls).toEqual([
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
			{ id: "project-1", name: "project", kind: "PROJECT_KIND_USER" },
		];
		harness.platform.services = [
			{
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
				},
			},
		];
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-project-1",
			serviceId: "service-1",
			repositorySelector: "",
			trackedRef: "",
			dockerfilePath: "",
			contextDir: ".",
			hostname: "hello.example.test",
		});

		const home = await homeOperations.loadDashboardHome(harness.runtime);

		expect(home?.project?.id).toBe("project-1");
		expect(home?.services).toHaveLength(1);
		expect(home?.service?.id).toBe("service-1");
		expect(home?.serviceStatus).toBeUndefined();
		expect(home?.domainBindings).toEqual([]);
		expect(harness.platform.getServiceStatusCalls).toEqual([]);
		expect(harness.platform.listDomainBindingsCalls).toEqual([]);
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
			{
				id: "project-1",
				name: "brisk-harbor",
				kind: "PROJECT_KIND_USER",
			},
		];
		harness.platform.services = [
			{
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					source: {
						provider: "github",
						repositorySelector: "octocat/hello",
						trackedRef: "main",
						buildRecipe: {
							dockerfilePath: "Dockerfile",
							contextDir: ".",
						},
					},
					runtime: {
						env: {},
						cpuMillis: 250,
						memoryMebibytes: 256,
						ports: [{ port: 8080, primary: true }],
					},
				},
			},
		];
		// Pre-link the project in the onboarding draft so it is reused by ID
		await harness.store.saveOnboardingDraft("user-1", {
			projectId: "project-1",
			environmentId: "environment-project-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
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

		expect(harness.platform.updateServiceCalls).toHaveLength(0);
		expect(harness.platform.createServiceCalls).toEqual([
			{
				user: {
					id: "user-1",
					email: "user@example.com",
				},
				environmentId: "environment-project-1",
				name: "talented-harmony",
				spec: {
					source: {
						provider: "github",
						repositorySelector: "octocat/hello",
						trackedRef: "main",
						buildRecipe: {
							dockerfilePath: "Dockerfile",
							contextDir: ".",
						},
					},
					desiredReplicaCount: 1,
					runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
				},
			},
		]);
		expect(draft.serviceId).toBe("service-2");
	});

	it("seeds service runtime ports from repository inspection", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.nextRepositoryInspection = {
			accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
			defaultBranch: "main",
			dockerfileCandidates: ["Dockerfile"],
			recommendedBuildRecipe: {
				dockerfilePath: "Dockerfile",
				contextDir: ".",
			},
			recommendedPorts: [3000, 8080],
		};

		await onboarding.createServiceFastFromSession(harness.runtime, {
			repositorySelector: "octocat/hello",
			serviceName: "talented-harmony",
		});

		expect(harness.platform.createServiceCalls[0].spec.runtime.ports).toEqual([
			{ port: 3000, primary: true },
			{ port: 8080, primary: false },
		]);
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
		expect(result.serviceStatus?.service.id).toBe("service-1");
		expect(result.onboarding).toMatchObject({
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
		});
		expect(harness.platform.createServiceCalls[0].spec.runtime).toMatchObject({
			cpuMillis: 250,
			memoryMebibytes: 256,
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
		expect(harness.platform.createProjectCalls).toEqual([]);
		expect(harness.platform.linkGitHubRepositoryCalls).toEqual([]);
		expect(harness.platform.createServiceCalls).toEqual([]);
	});

	it("forwards rich service log filters to the platform", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.serviceLogs = [
			{
				allocationId: "",
				agentId: "",
				stream: "deploy",
				rolloutGeneration: 1,
				sequence: 1,
				line: "initializing service",
				logType: "SERVICE_LOG_TYPE_DEPLOY",
				buildId: "build-1",
				stage: "initialization",
			},
		];

		const logs = await services.listServiceLogsFromSession(harness.runtime, {
			serviceId: "service-1",
			limit: 500,
			logType: "SERVICE_LOG_TYPE_DEPLOY",
			buildId: "build-1",
			search: "initializing",
		});

		expect(logs).toHaveLength(1);
		expect(harness.platform.listServiceLogsCalls[0]).toMatchObject({
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
			{
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "old-name",
				spec: {
					source: {
						provider: "github",
						repositorySelector: "octocat/hello",
						trackedRef: "main",
						buildRecipe: {
							dockerfilePath: "Dockerfile",
							contextDir: ".",
						},
					},
					runtime: {
						env: {},
						cpuMillis: 250,
						memoryMebibytes: 256,
						ports: [{ port: 8080, primary: true }],
						healthCheck: { path: "/ready", port: 8080, timeoutSeconds: 3 },
						volumeName: "data",
					},
				},
			},
		];
		harness.platform.environments = [
			{
				id: "environment-project-1",
				projectId: "project-1",
				name: "Production",
				kind: "persistent",
				isProduction: true,
			},
		];

		const updated = await services.updateServiceFromSession(harness.runtime, {
			serviceId: "service-1",
			serviceName: "talented-harmony",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
		});

		expect(updated.name).toBe("talented-harmony");
		expect(harness.platform.updateServiceCalls[0]).toMatchObject({
			serviceId: "service-1",
			name: "talented-harmony",
			spec: {
				runtime: {
					healthCheck: {
						path: "/ready",
						port: 8080,
						timeoutSeconds: 3,
					},
					volumeName: "data",
				},
			},
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
			restart: {
				policy: "RESTART_POLICY_NEVER",
				maxRestarts: 5,
				windowSeconds: 300,
			},
		});

		expect(harness.github.listRepositoriesCalls).toEqual([]);
		expect(harness.platform.linkGitHubRepositoryCalls).toEqual([]);
		expect(harness.platform.updateServiceCalls).toHaveLength(1);
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
			{
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "persistent",
				isProduction: true,
			},
		];

		await services.updateServiceFromSession(harness.runtime, {
			serviceId: "service-1",
			repositorySelector: "octocat/other",
		});

		expect(harness.github.listRepositoriesCalls).toEqual([
			"github-access-token",
		]);
		expect(harness.platform.linkGitHubRepositoryCalls).toMatchObject([
			{
				projectId: "project-1",
				repositorySelector: "octocat/other",
				githubUserAccessToken: "github-access-token",
			},
		]);
		expect(harness.platform.updateServiceCalls).toHaveLength(1);
	});

	it("deletes a service through the platform", async () => {
		const harness = createDashboardTestHarness();
		await auth.completeAuthCallback(harness.runtime, {
			userId: "user-1",
			email: "user@example.com",
			redirectTo: "/",
		});
		harness.platform.services = [
			{
				id: "service-1",
				environmentId: "environment-project-1",
				projectId: "project-1",
				name: "doomed",
			},
		];

		await services.deleteServiceFromSession(harness.runtime, {
			serviceId: "service-1",
		});

		expect(harness.platform.deleteServiceCalls).toMatchObject([
			{ serviceId: "service-1" },
		]);
		expect(harness.platform.services).toEqual([]);
	});
});

function repositoryBackedService() {
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "hello",
		spec: {
			source: {
				provider: "github" as const,
				repositorySelector: "octocat/hello",
				trackedRef: "main",
				buildRecipe: { dockerfilePath: "Dockerfile", contextDir: "." },
			},
			runtime: {
				env: {},
				cpuMillis: 250,
				memoryMebibytes: 256,
				ports: [],
			},
		},
	};
}
