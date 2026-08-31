import { useLayoutEffect, useState } from "react";

const PULSE_PERIOD_MS = 1400;

/**
 * Wall-clock `animation-delay` so independently mounted pulses stay in phase.
 * The first render is always `0ms` so SSR HTML matches the hydrated tree;
 * the phase is applied in layout after mount, before paint.
 */
export function usePulseDelay(active: boolean): string {
	const [delay, setDelay] = useState("0ms");

	useLayoutEffect(() => {
		if (!active) {
			setDelay("0ms");
			return;
		}
		setDelay(`${-(Date.now() % PULSE_PERIOD_MS)}ms`);
	}, [active]);

	return delay;
}
