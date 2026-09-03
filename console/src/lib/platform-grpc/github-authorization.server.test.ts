import { beforeEach, describe, expect, it, vi } from "vitest";
import type { PlatformRuntimeConfig } from "#/lib/platform-grpc/client.server";
import { createPlatformGateway } from "#/lib/platform-grpc/gateway.server";

const linkGitHubRepository = vi.hoisted(() => vi.fn());

vi.mock("#/lib/platform-grpc/client.server", () => ({
	getPlatformClient: () => ({ linkGitHubRepository }),
	getOpsClient: () => ({}),
	userAssertionMetadata: () => ({}),
	toPlatformGatewayError: (_operation: string, cause: unknown) => cause,
}));

function runtime(): PlatformRuntimeConfig {
	return {
		controlPlaneAddress: "127.0.0.1:9443",
		controlPlaneServerName: "controlplane.local",
		controlPlaneCA: Buffer.from("ca"),
		controlPlaneCert: Buffer.from("cert"),
		controlPlaneKey: Buffer.from("key"),
		userAssertionSecret: "test-user-assertion-secret-that-is-long-enough",
	};
}

describe("GitHub repository authorization gateway", () => {
	beforeEach(() => {
		linkGitHubRepository.mockReset();
	});

	it("passes the OAuth token only in the transient link request", async () => {
		const token = "github-user-token-sensitive";
		const log = vi.spyOn(console, "log").mockImplementation(() => undefined);
		const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
		const error = vi
			.spyOn(console, "error")
			.mockImplementation(() => undefined);
		linkGitHubRepository.mockResolvedValue({
			accessState: 1,
			defaultBranch: "main",
			dockerfileCandidates: ["Dockerfile"],
			recommendedPorts: [8080],
		});

		const user = {
			id: "user-1",
			subject: "user-1",
			email: "user@example.com",
		};
		const result = await createPlatformGateway(runtime()).linkGitHubRepository(
			user,
			{
				projectId: "project-1",
				repositorySelector: "private/secret",
				githubUserAccessToken: token,
			},
		);

		expect(linkGitHubRepository).toHaveBeenCalledWith(
			expect.objectContaining({
				projectId: "project-1",
				repositorySelector: "private/secret",
				githubUserAccessToken: token,
			}),
			{ headers: {} },
		);
		expect(JSON.stringify(result)).not.toContain(token);
		expect(log).not.toHaveBeenCalled();
		expect(warn).not.toHaveBeenCalled();
		expect(error).not.toHaveBeenCalled();

		log.mockRestore();
		warn.mockRestore();
		error.mockRestore();
	});
});
