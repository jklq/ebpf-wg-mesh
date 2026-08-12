import { describe, expect, it } from "vitest";

import {
	decodeIndexedServiceStatusResponse,
	decodeIndexedServicesResponse,
	decodeListServiceDeploymentsResponse,
	decodeListServicesResponse,
	encodeCreateServiceRequest,
	encodeUpdateServiceRequest,
} from "#/lib/platform-grpc/codec.server";

describe("platform grpc codec", () => {
	it("decodes blocking-query timeouts without payloads", () => {
		expect(
			decodeIndexedServicesResponse({ index: "42", notModified: true }),
		).toEqual({ index: 42, notModified: true, services: undefined });
		expect(
			decodeIndexedServiceStatusResponse({
				index: "42",
				notModified: true,
			}),
		).toEqual({ index: 42, notModified: true, status: undefined });
	});

	it("encodes mandatory service resource requests", () => {
		const request = encodeCreateServiceRequest({
			environmentId: "environment-1",
			name: "web",
			spec: {
				runtime: {
					env: {},
					cpuMillis: 500,
					memoryMebibytes: 768,
					ports: [],
				},
			},
		});

		expect(request.service.spec.runtime).toMatchObject({
			cpuMillis: 500,
			memoryMebibytes: 768,
		});
	});

	it("decodes deployment history with build commit metadata", () => {
		const response = decodeListServiceDeploymentsResponse({
			deployments: [
				{
					id: "rollout:service-1:2",
					serviceId: "service-1",
					rolloutGeneration: "2",
					specRevision: "3",
					reason: "build-success",
					createdAt: "2026-04-24T11:00:00Z",
					isCurrent: true,
					build: {
						buildId: "build-1",
						state: "BUILD_STATE_SUCCEEDED",
						commitSha: "abc123",
						commitMessage: "Persist dashboard deployment history",
						commitAuthor: "Alice",
						queuedAt: "2026-04-24T10:59:00Z",
						failureReason: "",
						stages: [],
					},
				},
			],
		});

		expect(response.deployments).toEqual([
			expect.objectContaining({
				id: "rollout:service-1:2",
				rolloutGeneration: 2,
				specRevision: 3,
				isCurrent: true,
				build: expect.objectContaining({
					buildId: "build-1",
					commitMessage: "Persist dashboard deployment history",
					commitAuthor: "Alice",
				}),
			}),
		]);
	});

	it("preserves HTTP rollout readiness configuration", () => {
		const [service] = decodeListServicesResponse({
			services: [
				{
					id: "service-1",
					environmentId: "environment-1",
					name: "web",
					internalHostname: "accurate-reflection.mesh.internal",
					specRevision: 2,
					rolloutGeneration: 1,
					spec: {
						runtime: {
							env: {},
							ports: [{ port: 8080, primary: true }],
							healthCheck: {
								type: "TYPE_HTTP",
								path: "/ready",
								port: 8080,
								timeoutSeconds: 3,
							},
						},
					},
				},
			],
		});
		if (!service.spec) {
			throw new Error("decoded service spec is missing");
		}
		expect(service.internalHostname).toBe(
			"accurate-reflection.mesh.internal",
		);

		expect(service.spec.runtime.healthCheck).toEqual({
			path: "/ready",
			port: 8080,
			timeoutSeconds: 3,
		});
		expect(
			encodeUpdateServiceRequest({
				serviceId: service.id,
				spec: service.spec,
			}).service.spec.runtime.healthCheck,
		).toEqual({
			type: "TYPE_HTTP",
			path: "/ready",
			port: 8080,
			timeoutSeconds: 3,
		});
	});
});
