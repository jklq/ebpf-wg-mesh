import { createFileRoute } from "@tanstack/react-router";

import { PlatformGatewayError } from "#/lib/dashboard/core/types.server";

export const Route = createFileRoute("/webhooks/github")({
	server: {
		handlers: {
			POST: async ({ request }: { request: Request }) => {
				const payload = new Uint8Array(await request.arrayBuffer());
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
					const service = await import("#/lib/dashboard/server");
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
		}
	}
	return new Response("enqueue webhook", { status: 500 });
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
