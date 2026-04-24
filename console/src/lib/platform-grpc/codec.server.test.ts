import { describe, expect, it } from "vitest";

import type { DashboardServiceSpec } from "#/lib/dashboard/core/types.server";
import {
	decodeDomainBindingMessage,
	decodeInspectSourceResponse,
	decodeListServiceLogsResponse,
	decodeServiceMessage,
	decodeServiceStatusMessage,
	encodeCreateServiceRequest,
	encodeIngestGitHubWebhookRequest,
	encodeUpdateServiceRequest,
} from "#/lib/platform-grpc/codec.server";

describe("platform grpc gateway", () => {
	it("encodes runtime ports when creating a source-backed service", () => {
		const spec: DashboardServiceSpec = {
			source: {
				provider: "github",
				repositorySelector: "octocat/hello",
				trackedRef: "main",
				buildRecipe: {
					dockerfilePath: "Dockerfile",
					contextDir: ".",
				},
			},
			runtime: { ports: [{ port: 8080, primary: true }] },
		};

		const request = encodeCreateServiceRequest({
			projectId: "project-1",
			name: "hello",
			spec,
		});

		expect(request.service.spec.runtime).toEqual({
			ports: [{ port: 8080, primary: true }],
		});
		expect(request.service.spec.source.sourceSpec.buildRecipe).toEqual({
			dockerfilePath: "Dockerfile",
			contextDir: ".",
		});
	});

	it("preserves runtime ports when updating a source-backed service", () => {
		const spec: DashboardServiceSpec = {
			source: {
				provider: "github",
				repositorySelector: "octocat/hello",
				trackedRef: "main",
			},
			runtime: { ports: [{ port: 3001, primary: true }] },
		};

		const request = encodeUpdateServiceRequest({
			projectId: "project-1",
			serviceId: "service-1",
			name: "talented-harmony",
			spec,
		});

		expect(request.service.name).toBe("talented-harmony");
		expect(request.service.spec.runtime).toEqual({
			ports: [{ port: 3001, primary: true }],
		});
	});

	it("decodes ports, target ports, and allocation routing status", () => {
		expect(
			decodeInspectSourceResponse({
				accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
				defaultBranch: "main",
				dockerfileCandidates: ["Dockerfile"],
				recommendedPorts: [8080, 9090],
			}).recommendedPorts,
		).toEqual([8080, 9090]);

		expect(
			decodeServiceMessage({
				id: "service-1",
				projectId: "project-1",
				name: "hello",
				spec: {
					runtime: { ports: [{ port: 8080, primary: true }] },
					source: {
						sourceSpec: {
							provider: "github",
							repositorySelector: "octocat/hello",
							trackedRef: "main",
						},
					},
				},
			}).spec?.runtime.ports,
		).toEqual([{ port: 8080, primary: true }]);

		expect(
			decodeDomainBindingMessage({
				hostname: "app.example.test",
				projectId: "project-1",
				serviceId: "service-1",
				targetPort: 3000,
			}).targetPort,
		).toBe(3000);

		expect(
			decodeServiceStatusMessage({
				service: {
					id: "service-1",
					projectId: "project-1",
					name: "hello",
				},
				allocation: {
					phase: "Healthy",
					message: "",
					allocationIp: "fd00::10",
					healthy: true,
					healthyPorts: [3000],
				},
			}).allocation,
		).toMatchObject({
			allocationIp: "fd00::10",
			healthyPorts: [3000],
		});
	});

	it("encodes GitHub webhook signatures using the proto field name", () => {
		const request = encodeIngestGitHubWebhookRequest({
			deliveryId: "delivery-1",
			eventType: "push",
			signature256: "sha256=abc123",
			payload: new Uint8Array([1, 2, 3]),
		});

		expect(request).toEqual({
			deliveryId: "delivery-1",
			eventType: "push",
			signature_256: "sha256=abc123",
			payload: new Uint8Array([1, 2, 3]),
		});
	});

	it("decodes build commit metadata and deployment stages", () => {
		const service = decodeServiceMessage({
			id: "service-1",
			projectId: "project-1",
			name: "hello",
			latestBuild: {
				buildId: "build-1",
				state: "BUILD_STATE_RUNNING",
				commitSha: "abc1234567",
				imageDigest: "",
				failureReason: "",
				commitMessage: "Ship rollout view",
				commitAuthor: "Octo Cat",
				stages: [
					{
						key: "build",
						label: "Build",
						detail: "Building image",
						state: "DEPLOYMENT_STAGE_STATE_RUNNING",
						startedAt: { seconds: 1_700_000_000 },
					},
				],
			},
		});

		expect(service.latestBuild).toMatchObject({
			commitMessage: "Ship rollout view",
			commitAuthor: "Octo Cat",
			stages: [
				{
					key: "build",
					label: "Build",
					detail: "Building image",
					state: "running",
				},
			],
		});
		expect(service.latestBuild?.stages?.[0]?.startedAt).toBeInstanceOf(Date);
	});

	it("decodes service log type, build id, and stage", () => {
		const response = decodeListServiceLogsResponse({
			lines: [
				{
					observedAt: { seconds: 1_700_000_000 },
					projectId: "project-1",
					serviceId: "service-1",
					allocationId: "",
					agentId: "",
					stream: "deploy",
					rolloutGeneration: 1,
					sequence: "2",
					line: "initializing service",
					logType: "SERVICE_LOG_TYPE_DEPLOY",
					buildId: "build-1",
					stage: "initialization",
				},
			],
		});

		expect(response.lines[0]).toMatchObject({
			logType: "deploy",
			buildId: "build-1",
			stage: "initialization",
			sequence: 2,
		});
	});

	it("decodes missing service log lines as an empty list", () => {
		expect(decodeListServiceLogsResponse({}).lines).toEqual([]);
	});
});
