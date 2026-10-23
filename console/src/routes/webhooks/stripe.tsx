import { Code } from "@connectrpc/connect";
import { createFileRoute } from "@tanstack/react-router";
import { createServerOnlyFn } from "@tanstack/react-start";

import { PlatformGatewayError } from "#/lib/dashboard/core/types.server";
import type { IngestStripeWebhookInput } from "#/lib/platform-grpc/gateway.server";
import {
	readBoundedRequestBody,
	WebhookPayloadTooLargeError,
} from "#/lib/webhook-body.server";

/** Mirrors the control plane's billing.MaxWebhookBytes. */
export const maxStripeWebhookPayloadBytes = 256 << 10;

export const Route = createFileRoute("/webhooks/stripe")({
	server: {
		handlers: {
			POST: async ({ request }: { request: Request }) =>
				stripeWebhookResponse(request, async (input) =>
					(await import("#/lib/dashboard/server")).forwardStripeWebhook(input),
				),
		},
	},
	component: StripeWebhookPage,
});

function StripeWebhookPage() {
	return null;
}

/**
 * Forwards a Stripe delivery to the control plane byte for byte. Signature
 * verification and every billing decision happen in Go; success is returned
 * only after the control plane has durably accepted the event, so Stripe
 * retries anything else.
 */
export const stripeWebhookResponse = createServerOnlyFn(
	async function stripeWebhookResponse(
		request: Request,
		forward: (input: IngestStripeWebhookInput) => Promise<void>,
	): Promise<Response> {
		let payload: Uint8Array;
		try {
			payload = await readBoundedRequestBody(
				request,
				maxStripeWebhookPayloadBytes,
			);
		} catch (error) {
			if (error instanceof WebhookPayloadTooLargeError)
				return new Response(error.message, { status: 413 });
			return new Response("read webhook body", { status: 400 });
		}
		const signature = request.headers.get("stripe-signature") ?? "";
		try {
			await forward({ signature, payload });
			return new Response(null, { status: 200 });
		} catch (error) {
			console.warn("stripe webhook request failed", {
				payload_bytes: payload.length,
				grpc_code:
					error instanceof PlatformGatewayError ? error.grpcCode : undefined,
				error: error instanceof Error ? error.message : String(error),
			});
			if (error instanceof PlatformGatewayError) {
				switch (error.grpcCode) {
					case Code.Unauthenticated:
						return new Response("invalid signature", { status: 400 });
					case Code.ResourceExhausted:
						return new Response("webhook payload too large", { status: 413 });
					case Code.FailedPrecondition:
						return new Response("stripe webhooks are not configured", {
							status: 503,
						});
				}
			}
			return new Response("ingest webhook", { status: 500 });
		}
	},
);
