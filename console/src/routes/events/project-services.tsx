import { createFileRoute } from "@tanstack/react-router";

// Keep under Bun.serve's default 10s idleTimeout so quiet SSE streams stay open.
const SERVICES_HEARTBEAT_INTERVAL_MS = 5_000;
const SERVICES_WAIT_TIMEOUT_SECONDS = 300;
const SERVICES_RETRY_DELAY_MS = 1_000;

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
							loadServices: (waitIndex) =>
								svc.waitForProjectServicesFromSession({
									projectId,
									waitIndex,
									waitTimeoutSeconds: SERVICES_WAIT_TIMEOUT_SECONDS,
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
	loadServices: (waitIndex: number) => Promise<{
		index: number;
		notModified: boolean;
		services?: unknown;
	}>;
	signal: AbortSignal;
}): ReadableStream<Uint8Array> {
	const encoder = new TextEncoder();
	let closed = false;
	let lastIndex = 0;
	let timer: ReturnType<typeof setTimeout> | undefined;
	let heartbeat: ReturnType<typeof setInterval> | undefined;
	let closeStream: (() => void) | undefined;

	const clearTimer = () => {
		if (!timer) return;
		clearTimeout(timer);
		timer = undefined;
	};
	const clearHeartbeat = () => {
		if (!heartbeat) return;
		clearInterval(heartbeat);
		heartbeat = undefined;
	};

	return new ReadableStream<Uint8Array>({
		start(controller) {
			const close = () => {
				if (closed) return;
				closed = true;
				clearTimer();
				clearHeartbeat();
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

			const scheduleRetry = () => {
				if (closed) return;
				clearTimer();
				timer = setTimeout(() => {
					void publish();
				}, SERVICES_RETRY_DELAY_MS);
			};

			const publish = async () => {
				if (closed || signal.aborted) {
					close();
					return;
				}
				try {
					const result = await loadServices(lastIndex);
					lastIndex = result.index;
					if (!result.notModified && result.services !== undefined) {
						enqueue(
							`id: ${result.index}\nevent: services\ndata: ${JSON.stringify(result.services)}\n\n`,
						);
					}
					if (!closed) void publish();
				} catch (error) {
					enqueue(
						`event: services-error\ndata: ${JSON.stringify({
							message: error instanceof Error ? error.message : "stream failed",
						})}\n\n`,
					);
					if (!closed) scheduleRetry();
				}
			};

			signal.addEventListener("abort", close, { once: true });
			enqueue(`retry: ${SERVICES_RETRY_DELAY_MS}\n\n`);
			heartbeat = setInterval(() => {
				enqueue("event: ping\ndata: {}\n\n");
			}, SERVICES_HEARTBEAT_INTERVAL_MS);
			void publish();
		},
		cancel() {
			closeStream?.();
		},
	});
}
