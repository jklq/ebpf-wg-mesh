import { describe, expect, it } from "vitest";

import { decodeListServiceDeploymentsResponse } from "#/lib/platform-grpc/codec.server";

describe("platform grpc codec", () => {
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
});
