import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/events/service-status")({
	server: {
		handlers: {
			GET: async ({ request }: { request: Request }) => {
				const url = new URL(request.url);
				const projectId = url.searchParams.get("projectId")?.trim() ?? "";
				const serviceId = url.searchParams.get("serviceId")?.trim() ?? "";

				if (!projectId || !serviceId) {
					return new Response("missing projectId or serviceId", {
						status: 400,
					});
				}

				try {
					const svc = await import("#/lib/dashboard/server");
					return new Response(
						createServiceStatusEventStream({
							loadStatus: () =>
								svc.getServiceStatusFromSession({
									projectId,
									serviceId,
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
	component: ServiceStatusEventPage,
});

function ServiceStatusEventPage() {
	return null;
}

function createServiceStatusEventStream({
	loadStatus,
	signal,
}: {
	loadStatus: () => Promise<unknown>;
	signal: AbortSignal;
}): ReadableStream<Uint8Array> {
	const encoder = new TextEncoder();
	let closed = false;
	let lastPayload = "";
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
				}, 2000);
			};

			const publish = async () => {
				if (closed || signal.aborted) {
					close();
					return;
				}
				try {
					const status = await loadStatus();
					const payload = JSON.stringify(status);
					if (payload !== lastPayload) {
						enqueue(`event: status\ndata: ${payload}\n\n`);
						lastPayload = payload;
					} else {
						enqueue("event: ping\ndata: {}\n\n");
					}
				} catch (error) {
					enqueue(
						`event: status-error\ndata: ${JSON.stringify({
							message: error instanceof Error ? error.message : "stream failed",
						})}\n\n`,
					);
				} finally {
					if (!closed) schedule();
				}
			};

			signal.addEventListener("abort", close, { once: true });
			enqueue("retry: 2000\n\n");
			void publish();
		},
		cancel() {
			closeStream?.();
		},
	});
}
