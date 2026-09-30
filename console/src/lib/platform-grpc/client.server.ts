import { ConnectError, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-node";

import type { DashboardUser } from "#/lib/dashboard/core/types.server";
import { PlatformGatewayError } from "#/lib/dashboard/core/types-errors.server";
import { formatError } from "#/lib/dashboard/core/utils.server";
import { createPlatformUserAssertion } from "#/lib/platform-grpc/user-assertion.server";

export interface PlatformRuntimeConfig {
	controlPlaneAddress: string;
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
	userAssertionSecret: string;
}

let transportInstance: Transport | undefined;

export function getTransport(runtime: PlatformRuntimeConfig): Transport {
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
	runtime: Pick<PlatformRuntimeConfig, "userAssertionSecret">,
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
		// rawMessage is the backend's own message without the "[code]" prefix, so errors read as written.
		message:
			cause instanceof ConnectError && cause.rawMessage
				? cause.rawMessage
				: formatError(cause),
		cause,
		grpcCode: connectCodeToGRPC(cause),
	});
}

function connectCodeToGRPC(cause: unknown): number | undefined {
	const code = (cause as { code?: unknown })?.code;
	return typeof code === "number" ? code : undefined;
}
