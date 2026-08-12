import { createFileRoute } from "@tanstack/react-router";

// Keep under Bun.serve's default 10s idleTimeout so quiet SSE streams stay open.
const STATUS_HEARTBEAT_INTERVAL_MS = 5_000;
const STATUS_WAIT_TIMEOUT_SECONDS = 300;
const STATUS_RETRY_DELAY_MS = 1_000;

export const Route = createFileRoute("/events/service-status")({
	server: {
		handlers: {
			GET: async ({ request }: { request: Request }) => {
				const url = new URL(request.url);
				const serviceId = url.searchParams.get("serviceId")?.trim() ?? "";

				if (!serviceId) {
					return new Response("missing serviceId", {
						status: 400,
					});
				}

				try {
					const svc = await import("#/lib/dashboard/entry.server");
					return new Response(
						createServiceStatusEventStream({
							loadStatus: (waitIndex) =>
								svc.waitForServiceStatusFromSession({
									serviceId,
									waitIndex,
									waitTimeoutSeconds: STATUS_WAIT_TIMEOUT_SECONDS,
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
	loadStatus: (waitIndex: number) => Promise<{
		index: number;
		notModified: boolean;
		status?: unknown;
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
				}, STATUS_RETRY_DELAY_MS);
			};

			const publish = async () => {
				if (closed || signal.aborted) {
					close();
					return;
				}
				try {
					const result = await loadStatus(lastIndex);
					lastIndex = result.index;
					if (!result.notModified && result.status !== undefined) {
						enqueue(
							`id: ${result.index}\nevent: status\ndata: ${JSON.stringify(result.status)}\n\n`,
						);
					}
					if (!closed) void publish();
				} catch (error) {
					enqueue(
						`event: status-error\ndata: ${JSON.stringify({
							message: error instanceof Error ? error.message : "stream failed",
						})}\n\n`,
					);
					if (!closed) scheduleRetry();
				}
			};

			signal.addEventListener("abort", close, { once: true });
			enqueue(`retry: ${STATUS_RETRY_DELAY_MS}\n\n`);
			heartbeat = setInterval(() => {
				enqueue("event: ping\ndata: {}\n\n");
			}, STATUS_HEARTBEAT_INTERVAL_MS);
			void publish();
		},
		cancel() {
			closeStream?.();
		},
	});
}
