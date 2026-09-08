import { createFileRoute } from "@tanstack/react-router";
import { indexedEventResponse } from "#/lib/dashboard/indexed-stream.server";

const WAIT_TIMEOUT_SECONDS = 300;

export const Route = createFileRoute("/events/service-status")({
	server: {
		handlers: {
			GET: async ({ request }: { request: Request }) => {
				const serviceId = new URL(request.url).searchParams
					.get("serviceId")
					?.trim();
				if (!serviceId) {
					return new Response("missing serviceId", { status: 400 });
				}
				try {
					const dashboard = await import("#/lib/dashboard/server");
					return indexedEventResponse({
						event: "status",
						signal: request.signal,
						load: async (waitIndex) => {
							const snapshot = await dashboard.waitForServiceStatusFromSession({
								serviceId,
								waitIndex,
								waitTimeoutSeconds: WAIT_TIMEOUT_SECONDS,
							});
							return {
								...snapshot,
								value: {
									status: snapshot.status,
									revision: snapshot.index,
								},
							};
						},
					});
				} catch {
					return new Response("unable to open stream", { status: 500 });
				}
			},
		},
	},
	component: EmptyEventPage,
});

function EmptyEventPage() {
	return null;
}
