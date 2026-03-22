import { createFileRoute } from "@tanstack/react-router";

import { PlatformGatewayError } from "#/lib/dashboard-core.server";

export const Route = createFileRoute("/webhooks/github")({
	server: {
		handlers: {
			POST: async ({ request }: { request: Request }) => {
				const payload = new Uint8Array(await request.arrayBuffer());
				const deliveryId = request.headers.get("x-github-delivery")?.trim() ?? "";
				const eventType = request.headers.get("x-github-event")?.trim() ?? "";
				const signature256 =
					request.headers.get("x-hub-signature-256")?.trim() ?? "";

				try {
					const service = await import("#/lib/dashboard.server");
					await service.forwardGitHubWebhook({
						deliveryId,
						eventType,
						signature256,
						payload,
					});
					return new Response(null, { status: 202 });
				} catch (error) {
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
				return new Response("github webhooks are not configured", { status: 503 });
		}
	}
	return new Response("enqueue webhook", { status: 500 });
}
