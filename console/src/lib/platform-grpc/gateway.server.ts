import type {
	DescMessage,
	DescMethodUnary,
	MessageJsonType,
} from "@bufbuild/protobuf";
import type { Transport } from "@connectrpc/connect";
import type { DashboardUser } from "#/lib/dashboard/core/types.server";
import { OpsService } from "#/lib/platform-gen/platform_pb";
import {
	getTransport,
	type PlatformRuntimeConfig,
	toPlatformGatewayError,
	userAssertionMetadata,
} from "#/lib/platform-grpc/client.server";
import {
	fromPlatformJson,
	type PlatformJson,
	toPlatformJson,
} from "#/lib/platform-json";

/** The generated protocol is the console's platform interface. */
export interface PlatformGateway {
	call<I extends DescMessage, O extends DescMessage>(
		method: DescMethodUnary<I, O>,
		user: DashboardUser,
		input: MessageJsonType<I>,
	): Promise<PlatformJson<O>>;
}

export function createAuthenticatedPlatform(
	transport: Transport,
	assertionSecret: string,
): PlatformGateway {
	return {
		async call(method, user, input) {
			try {
				const response = await transport.unary(
					method,
					undefined,
					undefined,
					userAssertionMetadata({ userAssertionSecret: assertionSecret }, user),
					fromPlatformJson(method.input, input),
				);
				// Canonical protobuf JSON preserves int64 precision, timestamps and oneofs.
				return toPlatformJson(method.output, response.message);
			} catch (cause) {
				throw toPlatformGatewayError(method.name, cause);
			}
		},
	};
}

export function createPlatformGateway(
	runtime: PlatformRuntimeConfig,
): PlatformGateway {
	return createAuthenticatedPlatform(
		getTransport(runtime),
		runtime.userAssertionSecret,
	);
}

export type IngestGitHubWebhookInput = {
	deliveryId: string;
	eventType: string;
	signature256: string;
	payload: Uint8Array;
};

export async function ingestGitHubWebhook(
	runtime: PlatformRuntimeConfig,
	input: IngestGitHubWebhookInput,
): Promise<void> {
	try {
		await getTransport(runtime).unary(
			OpsService.method.ingestGitHubWebhook,
			undefined,
			undefined,
			undefined,
			input,
		);
	} catch (cause) {
		throw toPlatformGatewayError("IngestGitHubWebhook", cause);
	}
}
