import { beforeEach, describe, expect, it, vi } from "vitest";

import type { PlatformRuntimeConfig } from "#/lib/platform-grpc/types.server";

const platformClient = {
	EnsurePrincipal: vi.fn(),
	ListProjects: vi.fn(),
	CreateProject: vi.fn(),
	InspectSource: vi.fn(),
	ListServices: vi.fn(),
	CreateService: vi.fn(),
	UpdateService: vi.fn(),
	GetService: vi.fn(),
	GetServiceStatus: vi.fn(),
	ListServiceLogs: vi.fn(),
	ListDomainBindings: vi.fn(),
	CreateDomainBinding: vi.fn(),
	UpdateDomainBinding: vi.fn(),
	DeleteDomainBinding: vi.fn(),
};

const opsClient = {
	IngestGitHubWebhook: vi.fn(),
};

const loadSync = vi.fn(() => ({}));
const loadPackageDefinition = vi.fn(() => ({
	platform: {
		v1: {
			PlatformService: vi.fn(() => platformClient),
			OpsService: vi.fn(() => opsClient),
		},
	},
}));

vi.mock("@grpc/proto-loader", () => ({
	loadSync,
}));

vi.mock("google-proto-files", () => ({
	default: {
		getProtoPath: () => "/tmp/google/protobuf/empty.proto",
	},
}));

vi.mock("@grpc/grpc-js", () => {
	class Metadata {
		#set(_key: string, _value: string) {}

		set(key: string, value: string) {
			this.#set(key, value);
		}
	}

	return {
		Metadata,
		loadPackageDefinition,
		credentials: {
			createSsl: vi.fn(() => ({})),
			createFromMetadataGenerator: vi.fn(() => ({})),
			combineChannelCredentials: vi.fn(() => ({})),
		},
	};
});

function runtime(): PlatformRuntimeConfig {
	return {
		controlPlaneAddress: "127.0.0.1:9443",
		controlPlaneServerName: "controlplane.local",
		controlPlaneCA: Buffer.from("ca"),
		controlPlaneCert: Buffer.from("cert"),
		controlPlaneKey: Buffer.from("key"),
	};
}

describe("platform grpc client", () => {
	beforeEach(() => {
		vi.resetModules();
		for (const fn of Object.values(platformClient)) {
			fn.mockReset();
		}
		for (const fn of Object.values(opsClient)) {
			fn.mockReset();
		}
		loadSync.mockClear();
		loadPackageDefinition.mockClear();
	});

	it("dispatches ListServiceLogs unary calls", async () => {
		platformClient.ListServiceLogs.mockImplementation(
			(_request, _metadata, callback) => {
				callback(null, { lines: [{ line: "hello" }] });
			},
		);

		const { unaryCall } = await import("#/lib/platform-grpc/client.server");
		const response = await unaryCall(runtime(), "ListServiceLogs", {
			projectId: "project-1",
			serviceId: "service-1",
			limit: 10,
		});

		expect(platformClient.ListServiceLogs).toHaveBeenCalledOnce();
		expect(response).toEqual({ lines: [{ line: "hello" }] });
	});

	it("dispatches update and delete domain binding unary calls", async () => {
		platformClient.UpdateDomainBinding.mockImplementation(
			(_request, _metadata, callback) => {
				callback(null, { hostname: "app.example.test" });
			},
		);
		platformClient.DeleteDomainBinding.mockImplementation(
			(_request, _metadata, callback) => {
				callback(null, {});
			},
		);

		const { unaryCall } = await import("#/lib/platform-grpc/client.server");

		await expect(
			unaryCall(runtime(), "UpdateDomainBinding", {
				projectId: "project-1",
				hostname: "app.example.test",
				binding: {
					serviceId: "service-1",
					targetPort: 3000,
				},
			}),
		).resolves.toEqual({ hostname: "app.example.test" });

		await expect(
			unaryCall(runtime(), "DeleteDomainBinding", {
				projectId: "project-1",
				hostname: "app.example.test",
			}),
		).resolves.toEqual({});

		expect(platformClient.UpdateDomainBinding).toHaveBeenCalledOnce();
		expect(platformClient.DeleteDomainBinding).toHaveBeenCalledOnce();
	});
});
