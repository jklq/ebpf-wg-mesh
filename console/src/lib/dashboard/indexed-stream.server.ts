const HEARTBEAT_INTERVAL_MS = 5_000;
const RETRY_DELAY_MS = 1_000;

export interface IndexedSnapshot<T> {
	index: number;
	notModified: boolean;
	value?: T;
}

export function indexedEventResponse<T>({
	event,
	load,
	signal,
}: {
	event: string;
	load: (after: number) => Promise<IndexedSnapshot<T>>;
	signal: AbortSignal;
}): Response {
	return new Response(indexedEventStream({ event, load, signal }), {
		headers: {
			"Content-Type": "text/event-stream; charset=utf-8",
			"Cache-Control": "no-cache, no-transform",
			Connection: "keep-alive",
			"X-Accel-Buffering": "no",
		},
	});
}

function indexedEventStream<T>({
	event,
	load,
	signal,
}: {
	event: string;
	load: (after: number) => Promise<IndexedSnapshot<T>>;
	signal: AbortSignal;
}): ReadableStream<Uint8Array> {
	const encoder = new TextEncoder();
	let closed = false;
	let lastIndex = 0;
	let retryTimer: ReturnType<typeof setTimeout> | undefined;
	let heartbeat: ReturnType<typeof setInterval> | undefined;
	let closeStream: (() => void) | undefined;

	return new ReadableStream<Uint8Array>({
		start(controller) {
			const clearTimers = () => {
				if (retryTimer) clearTimeout(retryTimer);
				if (heartbeat) clearInterval(heartbeat);
				retryTimer = undefined;
				heartbeat = undefined;
			};
			const close = () => {
				if (closed) return;
				closed = true;
				clearTimers();
				signal.removeEventListener("abort", close);
				try {
					controller.close();
				} catch {}
			};
			closeStream = close;

			const enqueue = (chunk: string) => {
				if (!closed) controller.enqueue(encoder.encode(chunk));
			};
			const scheduleRetry = () => {
				if (closed) return;
				if (retryTimer) clearTimeout(retryTimer);
				retryTimer = setTimeout(() => void publish(), RETRY_DELAY_MS);
			};
			const publish = async (): Promise<void> => {
				if (closed || signal.aborted) {
					close();
					return;
				}
				try {
					const snapshot = await load(lastIndex);
					lastIndex = snapshot.index;
					if (!snapshot.notModified && snapshot.value !== undefined) {
						enqueue(
							`id: ${snapshot.index}\nevent: ${event}\ndata: ${JSON.stringify(snapshot.value)}\n\n`,
						);
					}
					if (!closed) void publish();
				} catch (error) {
					enqueue(
						`event: ${event}-error\ndata: ${JSON.stringify({
							message: error instanceof Error ? error.message : "stream failed",
						})}\n\n`,
					);
					scheduleRetry();
				}
			};

			signal.addEventListener("abort", close, { once: true });
			enqueue(`retry: ${RETRY_DELAY_MS}\n\n`);
			heartbeat = setInterval(
				() => enqueue("event: ping\ndata: {}\n\n"),
				HEARTBEAT_INTERVAL_MS,
			);
			void publish();
		},
		cancel() {
			closeStream?.();
		},
	});
}
