import { createFileRoute } from "@tanstack/react-router";
import { indexedEventResponse } from "#/lib/dashboard/indexed-stream.server";

const WAIT_TIMEOUT_SECONDS = 300;

export const Route = createFileRoute("/events/environment-services")({
	server: {
		handlers: {
			GET: async ({ request }: { request: Request }) => {
				const environmentId = new URL(request.url).searchParams
					.get("environmentId")
					?.trim();
				if (!environmentId) {
					return new Response("missing environmentId", { status: 400 });
				}
				try {
					return indexedEventResponse({
						event: "services",
						signal: request.signal,
						load: async (waitIndex) => {
							const snapshot = await (
								await import("#/lib/dashboard/core/operations-services.server")
							).waitForEnvironmentServicesFromSession(
								(await import("#/lib/dashboard/server")).getDashboardRuntime(),
								{
									environmentId,
									waitIndex,
									waitTimeoutSeconds: WAIT_TIMEOUT_SECONDS,
								},
							);
							return {
								...snapshot,
								value: {
									services: snapshot.services,
									volumes: "volumes" in snapshot ? snapshot.volumes : undefined,
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
