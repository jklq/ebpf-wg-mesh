import { useEffect, useRef } from "react";

export function usePolling(
	callback: () => void | Promise<void>,
	{
		enabled,
		intervalMs,
		immediate = false,
	}: { enabled: boolean; intervalMs: number; immediate?: boolean },
) {
	const callbackRef = useRef(callback);
	callbackRef.current = callback;

	useEffect(() => {
		if (!enabled) return;
		let cancelled = false;
		let timeout: number | undefined;

		const schedule = () => {
			if (cancelled) return;
			timeout = window.setTimeout(run, intervalMs);
		};
		const run = async () => {
			try {
				await callbackRef.current();
			} catch {
				// The resource owns its error state. Polling continues after transient failures without unhandled promises.
			} finally {
				schedule();
			}
		};

		if (immediate) void run();
		else schedule();
		return () => {
			cancelled = true;
			if (timeout !== undefined) window.clearTimeout(timeout);
		};
	}, [enabled, immediate, intervalMs]);
}
