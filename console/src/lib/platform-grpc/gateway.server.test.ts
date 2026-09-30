import { createHmac } from "node:crypto";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { describe, expect, it, vi } from "vitest";
import { PlatformGatewayError } from "#/lib/dashboard/core/types.server";
import * as P from "#/lib/platform-gen/platform_pb";
import { createAuthenticatedPlatform } from "./gateway.server";

const secret = "console-platform-assertion-secret-over-32-bytes";
const user = { id: "user-1", email: "user@example.com" };

describe("authenticated generated platform adapter", () => {
	it("authenticates the exact generated request and preserves int64s, timestamps and selected oneofs", async () => {
		const updatedAt = new Date("2026-09-30T12:00:00Z");
		const controlPlane = createRouterTransport((router) =>
			router.service(P.PlatformService, {
				updateService(request, context) {
					const token = context.requestHeader.get("x-platform-user-assertion");
					expect(token).toBeTruthy();
					const [header, payload, signature] = token?.split(".") ?? [];
					expect(signature).toBe(
						createHmac("sha256", secret)
							.update(`${header}.${payload}`)
							.digest("base64url"),
					);
					expect(
						JSON.parse(Buffer.from(payload, "base64url").toString()),
					).toMatchObject({
						sub: user.id,
						iss: "managed-dashboard",
						aud: "controlplane",
					});
					expect(request.service?.spec?.source?.source).toEqual({
						case: "image",
						value: create(P.DirectImageSourceSchema, {
							image: "registry/app@sha256:abc",
						}),
					});
					expect(request.service?.spec?.runtime?.cpuMillis).toBe(
						9007199254740993n,
					);
					return {
						id: request.serviceId,
						spec: request.service?.spec,
						specRevision: 9007199254740993n,
						updatedAt: timestampFromDate(updatedAt),
						latestDeployment: {
							buildReused: true,
							state: P.DeploymentState.ACTIVE,
						},
					};
				},
			}),
		);
		const platform = createAuthenticatedPlatform(controlPlane, secret);
		const response = await platform.call(
			P.PlatformService.method.updateService,
			user,
			{
				serviceId: "service-1",
				service: {
					spec: {
						source: { image: { image: "registry/app@sha256:abc" } },
						runtime: { cpuMillis: "9007199254740993" },
					},
				},
			},
		);
		expect(response.specRevision).toBe("9007199254740993");
		expect(response.updatedAt).toBe("2026-09-30T12:00:00Z");
		expect(response.spec?.source?.image?.image).toBe("registry/app@sha256:abc");
		expect(response.latestDeployment).toMatchObject({
			buildReused: true,
			state: "DEPLOYMENT_STATE_ACTIVE",
		});
		expect(JSON.parse(JSON.stringify(response))).toEqual(response);
	});

	it("leaves absent spec/source/timestamps absent and emits defaults for present messages", async () => {
		const platform = createAuthenticatedPlatform(
			createRouterTransport((router) =>
				router.service(P.PlatformService, {
					getService: () => ({ id: "service-1" }),
				}),
			),
			secret,
		);
		const response = await platform.call(
			P.PlatformService.method.getService,
			user,
			{ serviceId: "service-1" },
		);
		expect(response).toMatchObject({
			name: "",
			specRevision: "0",
			unappliedChanges: [],
		});
		expect(response.spec).toBeUndefined();
		expect(response.sourceSummary).toBeUndefined();
		expect(response.createdAt).toBeUndefined();
	});

	it("uses backend error text and numeric gRPC codes", async () => {
		const platform = createAuthenticatedPlatform(
			createRouterTransport((router) =>
				router.service(P.PlatformService, {
					getService() {
						throw new ConnectError(
							"project access denied",
							Code.PermissionDenied,
						);
					},
				}),
			),
			secret,
		);
		await expect(
			platform.call(P.PlatformService.method.getService, user, {
				serviceId: "private",
			}),
		).rejects.toMatchObject({
			operation: "GetService",
			message: "project access denied",
			grpcCode: Code.PermissionDenied,
		});
	});

	it("forwards GitHub credentials only in the transient generated link request", async () => {
		const token = "github-user-token-sensitive";
		const handler = vi.fn((request) => {
			expect(request.githubUserAccessToken).toBe(token);
			return {
				accessState: P.SourceAccessState.AVAILABLE,
				defaultBranch: "main",
			};
		});
		const platform = createAuthenticatedPlatform(
			createRouterTransport((router) =>
				router.service(P.PlatformService, { linkGitHubRepository: handler }),
			),
			secret,
		);
		const log = vi.spyOn(console, "log");
		const warn = vi.spyOn(console, "warn");
		const error = vi.spyOn(console, "error");
		const response = await platform.call(
			P.PlatformService.method.linkGitHubRepository,
			user,
			{
				projectId: "project-1",
				repositorySelector: "owner/private",
				githubUserAccessToken: token,
			},
		);
		expect(handler).toHaveBeenCalledOnce();
		expect(JSON.stringify(response)).not.toContain(token);
		expect(log).not.toHaveBeenCalled();
		expect(warn).not.toHaveBeenCalled();
		expect(error).not.toHaveBeenCalled();
		vi.restoreAllMocks();
	});

	it("rejects an invalid user before the transport receives a request", async () => {
		const handler = vi.fn(() => ({}));
		const platform = createAuthenticatedPlatform(
			createRouterTransport((router) =>
				router.service(P.PlatformService, { getService: handler }),
			),
			secret,
		);
		await expect(
			platform.call(
				P.PlatformService.method.getService,
				{ ...user, id: " padded " },
				{},
			),
		).rejects.toBeInstanceOf(PlatformGatewayError);
		expect(handler).not.toHaveBeenCalled();
	});
});
