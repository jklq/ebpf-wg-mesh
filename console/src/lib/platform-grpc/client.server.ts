import { connect as connectTLS } from "node:tls";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
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

// A replica that does not complete TLS within this bound is treated as never
// having received the request, so a silent host failure costs one bounded
// attempt instead of the operating system's connect timeout.
const connectTimeoutMs = 2000;
// After a failed connection, requests prefer the other replicas for a while.
const unavailableMs = 5000;
const connectTimeoutCode = "ERR_CONTROL_PLANE_CONNECT_TIMEOUT";

// Choose a verified reachable replica before submitting the RPC. Never replay a
// submitted mutation: a transport error after sending may have external effects.
export async function selectControlPlane(
	runtime: PlatformRuntimeConfig,
	signal?: AbortSignal,
	excluded: ReadonlySet<string> = new Set(),
): Promise<string> {
	let failure: unknown;
	for (const address of runtime.controlPlaneAddresses) {
		if (signal?.aborted) throw signal.reason;
		if (excluded.has(address)) continue;
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
					createConnection: (authority: URL) => {
						const socket = connectTLS({
							host: authority.hostname.replace(/^\[|\]$/g, ""),
							port: Number(authority.port || 443),
							servername: runtime.controlPlaneServerName,
							ca: runtime.controlPlaneCA,
							cert: runtime.controlPlaneCert,
							key: runtime.controlPlaneKey,
							ALPNProtocols: ["h2"],
						});
						socket.setTimeout(connectTimeoutMs, () =>
							socket.destroy(
								Object.assign(new Error("control-plane connection timed out"), {
									code: connectTimeoutCode,
								}),
							),
						);
						socket.once("secureConnect", () => socket.setTimeout(0));
						return socket;
					},
				},
			}),
		]),
	);
	const unavailable = new Map<string, number>();
	// The native transport authenticates and pools HTTP/2 connections. Rotate
	// requests across peers without a second TLS handshake before every RPC.
	let nextReplica = 0;
	const transport: Transport = {
		unary: (...args) =>
			onReplica(
				runtime,
				args[1],
				nextReplica++ % candidates.size,
				unavailable,
				(address) => candidates.get(address)!.unary(...args),
			),
		stream: (...args) =>
			onReplica(
				runtime,
				args[1],
				nextReplica++ % candidates.size,
				unavailable,
				(address) => candidates.get(address)!.stream(...args),
			),
	};
	transports.set(runtime, transport);
	return transport;
}

// A request that a replica provably never processed may go to another replica:
// HTTP/2 refused its stream, as after a draining replica's GOAWAY, or no
// connection was established. Every other failure may follow an effect.
async function onReplica<T>(
	runtime: PlatformRuntimeConfig,
	signal: AbortSignal | undefined,
	start: number,
	unavailable: Map<string, number>,
	call: (address: string) => Promise<T>,
): Promise<T> {
	const refused = new Set<string>();
	const addresses = runtime.controlPlaneAddresses;
	const usable = (address: string) =>
		!refused.has(address) && (unavailable.get(address) ?? 0) <= Date.now();
	// Start from the rotation, skipping replicas that recently failed to connect
	// unless every replica did.
	let index = start;
	for (let n = 0; n < addresses.length && !usable(addresses[index]); n++) {
		index = (index + 1) % addresses.length;
	}
	for (;;) {
		if (signal?.aborted) throw signal.reason;
		const address = runtime.controlPlaneAddresses[index];
		if (!address) throw new Error("control-plane endpoints required");
		try {
			return await call(address);
		} catch (error) {
			refused.add(address);
			// Owner redirects are issued before the handler admits the call.
			// Follow only authenticated addresses from the configured seed set.
			const owner =
				error instanceof ConnectError && error.code === Code.FailedPrecondition
					? /^not the live owner; reconnect at (.+)$/.exec(
							error.rawMessage,
						)?.[1]
					: undefined;
			if (
				owner &&
				runtime.controlPlaneAddresses.includes(owner) &&
				!refused.has(owner)
			) {
				index = runtime.controlPlaneAddresses.indexOf(owner);
				continue;
			}
			if (!isUnprocessed(error)) {
				throw error;
			}
			unavailable.set(address, Date.now() + unavailableMs);
			if (refused.size >= addresses.length) {
				throw error;
			}
			const next = addresses.findIndex((_, n) =>
				usable(addresses[(index + 1 + n) % addresses.length]),
			);
			if (next >= 0) {
				index = (index + 1 + next) % addresses.length;
			} else {
				do {
					index = (index + 1) % addresses.length;
				} while (refused.has(addresses[index]));
			}
		}
	}
}

const unprocessedCodes = new Set([
	connectTimeoutCode,
	"ECONNREFUSED",
	"ERR_HTTP2_GOAWAY_SESSION",
	"ERR_HTTP2_INVALID_SESSION",
]);

export function isUnprocessed(error: unknown): boolean {
	for (let cause = error, depth = 0; cause && depth < 8; depth++) {
		if (
			cause instanceof ConnectError &&
			((cause.code === Code.Unavailable &&
				cause.rawMessage ===
					"http/2 stream closed with error code REFUSED_STREAM (0x7)") ||
				(cause.code === Code.Internal &&
					cause.rawMessage ===
						"Stream closed with error code NGHTTP2_REFUSED_STREAM"))
		) {
			return true;
		}
		const code = (cause as { code?: unknown }).code;
		if (typeof code === "string" && unprocessedCodes.has(code)) return true;
		cause = (cause as { cause?: unknown }).cause;
	}
	return false;
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
