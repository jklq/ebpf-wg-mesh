import { createFileRoute } from "@tanstack/react-router";

const SERVICES_POLL_INTERVAL_MS = 500;
const SERVICES_HEARTBEAT_INTERVAL_MS = 15_000;

export const Route = createFileRoute("/events/project-services")({
	server: {
		handlers: {
			GET: async ({ request }: { request: Request }) => {
				const url = new URL(request.url);
				const projectId = url.searchParams.get("projectId")?.trim() ?? "";

				if (!projectId) {
					return new Response("missing projectId", {
						status: 400,
					});
				}

				try {
					const svc = await import("#/lib/dashboard/server");
					return new Response(
						createProjectServicesEventStream({
							loadServices: () =>
								svc.listProjectServicesFromSession({
									projectId,
								}),
							signal: request.signal,
						}),
						{
							headers: {
								"Content-Type": "text/event-stream; charset=utf-8",
								"Cache-Control": "no-cache, no-transform",
								Connection: "keep-alive",
								"X-Accel-Buffering": "no",
							},
						},
					);
				} catch {
					return new Response("unable to open stream", { status: 500 });
				}
			},
		},
	},
	component: ProjectServicesEventPage,
});

function ProjectServicesEventPage() {
	return null;
}

function createProjectServicesEventStream({
	loadServices,
	signal,
}: {
	loadServices: () => Promise<unknown>;
	signal: AbortSignal;
}): ReadableStream<Uint8Array> {
	const encoder = new TextEncoder();
	let closed = false;
	let lastPayload = "";
	let lastHeartbeat = Date.now();
	let timer: ReturnType<typeof setTimeout> | undefined;
	let closeStream: (() => void) | undefined;

	const clearTimer = () => {
		if (!timer) return;
		clearTimeout(timer);
		timer = undefined;
	};

	return new ReadableStream<Uint8Array>({
		start(controller) {
			const close = () => {
				if (closed) return;
				closed = true;
				clearTimer();
				signal.removeEventListener("abort", close);
				try {
					controller.close();
				} catch {
					// Ignore repeated close attempts after the client disconnects.
				}
			};
			closeStream = close;

			const enqueue = (chunk: string) => {
				if (closed) return;
				controller.enqueue(encoder.encode(chunk));
			};

			const schedule = () => {
				if (closed) return;
				clearTimer();
				timer = setTimeout(() => {
					void publish();
				}, SERVICES_POLL_INTERVAL_MS);
			};

			const publish = async () => {
				if (closed || signal.aborted) {
					close();
					return;
				}
				try {
					const services = await loadServices();
					const payload = JSON.stringify(services);
					const now = Date.now();
					if (payload !== lastPayload) {
						enqueue(`event: services\ndata: ${payload}\n\n`);
						lastPayload = payload;
						lastHeartbeat = now;
					} else if (now - lastHeartbeat >= SERVICES_HEARTBEAT_INTERVAL_MS) {
						enqueue("event: ping\ndata: {}\n\n");
						lastHeartbeat = now;
					}
				} catch (error) {
					enqueue(
						`event: services-error\ndata: ${JSON.stringify({
							message: error instanceof Error ? error.message : "stream failed",
						})}\n\n`,
					);
				} finally {
					if (!closed) schedule();
				}
			};

			signal.addEventListener("abort", close, { once: true });
			enqueue("retry: 500\n\n");
			void publish();
		},
		cancel() {
			closeStream?.();
		},
	});
}
