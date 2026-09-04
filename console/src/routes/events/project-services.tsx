import { createFileRoute } from "@tanstack/react-router";
import { indexedEventResponse } from "#/lib/dashboard/indexed-stream.server";

const WAIT_TIMEOUT_SECONDS = 300;

export const Route = createFileRoute("/events/project-services")({
	server: {
		handlers: {
			GET: async ({ request }: { request: Request }) => {
				const projectId = new URL(request.url).searchParams
					.get("projectId")
					?.trim();
				if (!projectId) {
					return new Response("missing projectId", { status: 400 });
				}
				try {
					const dashboard = await import("#/lib/dashboard/server");
					return indexedEventResponse({
						event: "services",
						signal: request.signal,
						load: async (waitIndex) => {
							const snapshot =
								await dashboard.waitForProjectServicesFromSession({
									projectId,
									waitIndex,
									waitTimeoutSeconds: WAIT_TIMEOUT_SECONDS,
								});
							return { ...snapshot, value: snapshot.services };
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
