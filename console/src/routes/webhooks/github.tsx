import { createFileRoute } from "@tanstack/react-router";

import { PlatformGatewayError } from "#/lib/dashboard/core/types.server";

export const maxGitHubWebhookPayloadBytes = 2 << 20;

export class WebhookPayloadTooLargeError extends Error {}

export const Route = createFileRoute("/webhooks/github")({
	server: {
		handlers: {
			POST: async ({ request }: { request: Request }) => {
				let payload: Uint8Array;
				try {
					payload = await readBoundedRequestBody(request);
				} catch (error) {
					if (error instanceof WebhookPayloadTooLargeError) {
						return new Response(error.message, { status: 413 });
					}
					return new Response("read webhook body", { status: 400 });
				}
				const deliveryId =
					request.headers.get("x-github-delivery")?.trim() ?? "";
				const eventType = request.headers.get("x-github-event")?.trim() ?? "";
				const signature256 =
					request.headers.get("x-hub-signature-256")?.trim() ?? "";

				console.info("github webhook request received", {
					delivery_id: deliveryId,
					event_type: eventType,
					payload_bytes: payload.length,
				});

				try {
					const service = await import("#/lib/dashboard/entry.server");
					await service.forwardGitHubWebhook({
						deliveryId,
						eventType,
						signature256,
						payload,
					});
					console.info("github webhook request forwarded", {
						delivery_id: deliveryId,
						event_type: eventType,
						payload_bytes: payload.length,
					});
					return new Response(null, { status: 202 });
				} catch (error) {
					logWebhookForwardFailure(
						error,
						deliveryId,
						eventType,
						payload.length,
					);
					return mapWebhookError(error);
				}
			},
		},
	},
	component: GitHubWebhookPage,
});

function GitHubWebhookPage() {
	return null;
}

function mapWebhookError(error: unknown): Response {
	if (error instanceof PlatformGatewayError) {
		switch (error.grpcCode) {
			case 3:
				return new Response("missing delivery headers", { status: 400 });
			case 16:
				return new Response("invalid signature", { status: 401 });
			case 9:
				return new Response("github webhooks are not configured", {
					status: 503,
				});
			case 8:
				return new Response("webhook payload too large", { status: 413 });
		}
	}
	return new Response("enqueue webhook", { status: 500 });
}

export async function readBoundedRequestBody(
	request: Request,
	maxBytes = maxGitHubWebhookPayloadBytes,
): Promise<Uint8Array> {
	const contentLength = request.headers.get("content-length");
	if (contentLength !== null) {
		const parsed = Number(contentLength);
		if (!Number.isSafeInteger(parsed) || parsed < 0) {
			throw new Error("invalid content-length");
		}
		if (parsed > maxBytes) {
			throw new WebhookPayloadTooLargeError("webhook payload too large");
		}
	}
	if (!request.body) return new Uint8Array();

	const reader = request.body.getReader();
	const chunks: Uint8Array[] = [];
	let total = 0;
	try {
		for (;;) {
			const { done, value } = await reader.read();
			if (done) break;
			total += value.byteLength;
			if (total > maxBytes) {
				await reader.cancel("webhook payload too large");
				throw new WebhookPayloadTooLargeError("webhook payload too large");
			}
			chunks.push(value);
		}
	} finally {
		reader.releaseLock();
	}

	const payload = new Uint8Array(total);
	let offset = 0;
	for (const chunk of chunks) {
		payload.set(chunk, offset);
		offset += chunk.byteLength;
	}
	return payload;
}

function logWebhookForwardFailure(
	error: unknown,
	deliveryId: string,
	eventType: string,
	payloadBytes: number,
): void {
	if (error instanceof PlatformGatewayError) {
		console.warn("github webhook request failed", {
			delivery_id: deliveryId,
			event_type: eventType,
			payload_bytes: payloadBytes,
			grpc_code: error.grpcCode,
			error: error.message,
		});
		return;
	}
	console.warn("github webhook request failed", {
		delivery_id: deliveryId,
		event_type: eventType,
		payload_bytes: payloadBytes,
		error: error instanceof Error ? error.message : String(error),
	});
}
