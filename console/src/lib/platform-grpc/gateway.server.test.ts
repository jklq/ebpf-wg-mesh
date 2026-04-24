import { beforeEach, describe, expect, it, vi } from "vitest";

import { createPlatformGateway } from "#/lib/platform-grpc/gateway.server";
import type { PlatformRuntimeConfig } from "#/lib/platform-grpc/types.server";

const unaryCall = vi.fn();

vi.mock("#/lib/platform-grpc/client.server", () => ({
	getOpsClient: vi.fn(),
	toPlatformGatewayError: vi.fn(),
	unaryCall,
}));

function runtime(): PlatformRuntimeConfig {
	return {
		controlPlaneAddress: "127.0.0.1:9443",
		controlPlaneServerName: "controlplane.local",
		controlPlaneCA: Buffer.from("ca"),
		controlPlaneCert: Buffer.from("cert"),
		controlPlaneKey: Buffer.from("key"),
	};
}

describe("platform grpc gateway", () => {
	beforeEach(() => {
		unaryCall.mockReset();
	});

	it("loads service deployments through the grpc unary path", async () => {
		const user = {
			id: "user-1",
			subject: "user-1",
			email: "user@example.com",
		};
		unaryCall.mockResolvedValue({
			deployments: [
				{
					id: "build:build-1",
					serviceId: "service-1",
					rolloutGeneration: "2",
					specRevision: "0",
					reason: "build-failed",
					createdAt: "2026-04-24T11:00:00Z",
					isCurrent: true,
					build: {
						buildId: "build-1",
						state: "BUILD_STATE_FAILED",
						commitSha: "abc123",
						commitMessage: "Persist dashboard deployment history",
						commitAuthor: "Alice",
						queuedAt: "2026-04-24T10:59:00Z",
						failureReason: "docker build failed",
						stages: [],
					},
				},
			],
		});

		const gateway = createPlatformGateway(runtime());
		const deployments = await gateway.listServiceDeployments(user, {
			projectId: "project-1",
			serviceId: "service-1",
			limit: 5,
		});

		expect(unaryCall).toHaveBeenCalledWith(
			runtime(),
			"ListServiceDeployments",
			{ projectId: "project-1", serviceId: "service-1", limit: 5 },
			user,
		);
		expect(deployments).toEqual([
			expect.objectContaining({
				id: "build:build-1",
				rolloutGeneration: 2,
				isCurrent: true,
				build: expect.objectContaining({
					buildId: "build-1",
					commitMessage: "Persist dashboard deployment history",
				}),
			}),
		]);
	});
});
