import { Code } from "@connectrpc/connect";
import { describe, expect, it, vi } from "vitest";
import { PlatformGatewayError } from "#/lib/dashboard/core/types.server";
import type { IngestStripeWebhookInput } from "#/lib/platform-grpc/gateway.server";
import {
	maxStripeWebhookPayloadBytes,
	stripeWebhookResponse,
} from "#/routes/webhooks/stripe";

function delivery(body: BodyInit, signature = "t=1,v1=abc") {
	return new Request("https://console.example.test/webhooks/stripe", {
		method: "POST",
		headers: { "stripe-signature": signature },
		body,
	});
}

function gatewayError(grpcCode: number) {
	return new PlatformGatewayError({
		operation: "IngestStripeWebhook",
		message: "rejected",
		cause: undefined,
		grpcCode,
	});
}

describe("stripe webhook route", () => {
	it("forwards the raw bytes and signature unchanged and succeeds only once accepted", async () => {
		const raw = new Uint8Array([0x7b, 0x22, 0xff, 0x00, 0x0a, 0x7d]);
		let accept!: () => void;
		const forward = vi.fn(
			(_input: IngestStripeWebhookInput) =>
				new Promise<void>((resolve) => {
					accept = resolve;
				}),
		);
		let settled = false;
		const response = stripeWebhookResponse(
			delivery(raw, "t=1,v1=abc, v0=def"),
			forward,
		).then((result) => {
			settled = true;
			return result;
		});

		await vi.waitFor(() => expect(forward).toHaveBeenCalled());
		expect(forward.mock.calls[0][0]).toEqual({
			signature: "t=1,v1=abc, v0=def",
			payload: raw,
		});
		await Promise.resolve();
		expect(settled).toBe(false);

		accept();
		expect((await response).status).toBe(200);
	});

	it("maps control plane rejections so Stripe retries only what can succeed", async () => {
		const status = async (code: number) =>
			(
				await stripeWebhookResponse(delivery("{}"), () =>
					Promise.reject(gatewayError(code)),
				)
			).status;

		expect(await status(Code.Unauthenticated)).toBe(400);
		expect(await status(Code.ResourceExhausted)).toBe(413);
		expect(await status(Code.FailedPrecondition)).toBe(503);
		expect(await status(Code.Unavailable)).toBe(500);
	});

	it("rejects oversized bodies without forwarding them", async () => {
		const forward = vi.fn();
		const response = await stripeWebhookResponse(
			delivery(new Uint8Array(maxStripeWebhookPayloadBytes + 1)),
			forward,
		);

		expect(response.status).toBe(413);
		expect(forward).not.toHaveBeenCalled();
	});
});
