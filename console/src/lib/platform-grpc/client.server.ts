import { ConnectError, type Transport } from "@connectrpc/connect";
import { connect as connectTLS } from "node:tls";
import { createConnectTransport } from "@connectrpc/connect-node";

import type { DashboardUser } from "#/lib/dashboard/core/types.server";
import { PlatformGatewayError } from "#/lib/dashboard/core/types-errors.server";
import { formatError } from "#/lib/dashboard/core/utils.server";
import { createPlatformUserAssertion } from "#/lib/platform-grpc/user-assertion.server";

export interface PlatformRuntimeConfig {
	controlPlaneAddresses: string[];
	controlPlaneServerName: string;
	controlPlaneCA: Buffer;
	controlPlaneCert: Buffer;
	controlPlaneKey: Buffer;
	userAssertionSecret: string;
}

const transports = new WeakMap<PlatformRuntimeConfig, Transport>();

// Choose a verified reachable replica before submitting the RPC. Never replay a
// submitted mutation: a transport error after sending may have external effects.
export async function selectControlPlane(
	runtime: PlatformRuntimeConfig,
	signal?: AbortSignal,
): Promise<string> {
	let failure: unknown;
	for (const address of runtime.controlPlaneAddresses) {
		if (signal?.aborted) throw signal.reason;
		try {
			const target = new URL(`https://${address}`);
			await new Promise<void>((resolve, reject) => {
				const socket = connectTLS({
					host: target.hostname.replace(/^\[|\]$/g, ""),
					port: Number(target.port || 443),
					servername: runtime.controlPlaneServerName,
					ca: runtime.controlPlaneCA,
					cert: runtime.controlPlaneCert,
					key: runtime.controlPlaneKey,
					ALPNProtocols: ["h2"],
				});
				const abort = () =>
					socket.destroy(new Error("control-plane selection aborted"));
				signal?.addEventListener("abort", abort, { once: true });
				if (signal?.aborted) abort();
				socket.setTimeout(2000, () =>
					socket.destroy(new Error("control-plane TLS probe timed out")),
				);
				socket.once("error", (error) => {
					signal?.removeEventListener("abort", abort);
					reject(error);
				});
				socket.once("secureConnect", () => {
					signal?.removeEventListener("abort", abort);
					if (socket.alpnProtocol !== "h2") {
						socket.destroy();
						reject(new Error("control-plane endpoint does not support HTTP/2"));
						return;
					}
					socket.destroy();
					resolve();
				});
			});
			return address;
		} catch (error) {
			failure = error;
		}
	}
	throw failure ?? new Error("control-plane endpoints required");
}

export function getTransport(runtime: PlatformRuntimeConfig): Transport {
	const existing = transports.get(runtime);
	if (existing) return existing;
	const candidates = new Map(
		runtime.controlPlaneAddresses.map((address) => [
			address,
			createConnectTransport({
				baseUrl: `https://${address}`,
				httpVersion: "2",
				useBinaryFormat: true,
				nodeOptions: {
					ca: runtime.controlPlaneCA,
					cert: runtime.controlPlaneCert,
					key: runtime.controlPlaneKey,
					servername: runtime.controlPlaneServerName,
				},
			}),
		]),
	);
	const transport: Transport = {
		async unary(...args) {
			const address = await selectControlPlane(runtime, args[1]);
			return candidates.get(address)!.unary(...args);
		},
		async stream(...args) {
			const address = await selectControlPlane(runtime, args[1]);
			return candidates.get(address)!.stream(...args);
		},
	};
	transports.set(runtime, transport);
	return transport;
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
