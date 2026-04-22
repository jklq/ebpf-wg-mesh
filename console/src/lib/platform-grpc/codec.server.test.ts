import { describe, expect, it } from "vitest";

import type { DashboardSourceSpec } from "#/lib/dashboard/core/types.server";
import {
	encodeCreateServiceRequest,
	encodeIngestGitHubWebhookRequest,
	encodeUpdateServiceRequest,
} from "#/lib/platform-grpc/codec.server";

describe("platform grpc gateway", () => {
	it("injects a default runtime port when creating a source-backed service", () => {
		const source: DashboardSourceSpec = {
			provider: "github",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			buildRecipe: {
				dockerfilePath: "Dockerfile",
				contextDir: ".",
			},
		};

		const request = encodeCreateServiceRequest({
			projectId: "project-1",
			name: "hello",
			source,
		});

		expect(request.service.spec.runtime).toEqual({ containerPort: 8080 });
		expect(request.service.spec.source.sourceSpec.buildRecipe).toEqual({
			dockerfilePath: "Dockerfile",
			contextDir: ".",
		});
	});

	it("preserves an explicit runtime port when updating a source-backed service", () => {
		const source: DashboardSourceSpec = {
			provider: "github",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			containerPort: 3001,
		};

		const request = encodeUpdateServiceRequest({
			projectId: "project-1",
			serviceId: "service-1",
			source,
		});

		expect(request.service.spec.runtime).toEqual({ containerPort: 3001 });
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
});
