import { createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-node";

import type { DashboardUser } from "#/lib/dashboard/core/types.server";
import { PlatformGatewayError } from "#/lib/dashboard/core/types-errors.server";
import { formatError } from "#/lib/dashboard/core/utils.server";
import { OpsService, PlatformService } from "#/lib/platform-gen/platform_pb";
import { createPlatformUserAssertion } from "#/lib/platform-grpc/user-assertion.server";

export interface PlatformRuntimeConfig {
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
	userAssertionSecret: string;
}

export type PlatformClient = ReturnType<
	typeof createClient<typeof PlatformService>
>;

export type OpsClient = ReturnType<typeof createClient<typeof OpsService>>;

let transportInstance: Transport | undefined;
let clientInstance: PlatformClient | undefined;
let opsClientInstance: OpsClient | undefined;

export function getPlatformClient(
	runtime: PlatformRuntimeConfig,
): PlatformClient {
	clientInstance ??= createClient(PlatformService, getTransport(runtime));
	return clientInstance;
}

export function getOpsClient(runtime: PlatformRuntimeConfig): OpsClient {
	opsClientInstance ??= createClient(OpsService, getTransport(runtime));
	return opsClientInstance;
}

function getTransport(runtime: PlatformRuntimeConfig): Transport {
	transportInstance ??= createConnectTransport({
		baseUrl: `https://${runtime.controlPlaneAddress}`,
		httpVersion: "2",
		useBinaryFormat: true,
		nodeOptions: {
			ca: runtime.controlPlaneCA,
			cert: runtime.controlPlaneCert,
			key: runtime.controlPlaneKey,
			servername: runtime.controlPlaneServerName,
		},
	});
	return transportInstance;
}

export function userAssertionMetadata(
	runtime: PlatformRuntimeConfig,
	user: DashboardUser | undefined,
): Record<string, string> {
	if (!user) {
		return {};
	}
	return {
		"x-platform-user-assertion": createPlatformUserAssertion({
			secret: runtime.userAssertionSecret,
			userId: user.id,
		}),
	};
}

export function toPlatformGatewayError(
	operation: string,
	cause: unknown,
): PlatformGatewayError {
	if (cause instanceof PlatformGatewayError) {
		return cause;
	}
	return new PlatformGatewayError({
		operation,
		message: formatError(cause),
		cause,
		grpcCode: connectCodeToGRPC(cause),
	});
}

function connectCodeToGRPC(cause: unknown): number | undefined {
	const code = (cause as { code?: unknown })?.code;
	return typeof code === "number" ? code : undefined;
}
