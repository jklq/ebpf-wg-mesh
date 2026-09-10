import { useLayoutEffect, useState } from "react";

const PULSE_PERIOD_MS = 1400;

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
