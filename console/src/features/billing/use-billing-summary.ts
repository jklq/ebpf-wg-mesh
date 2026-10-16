import { useCallback, useEffect, useRef, useState } from "react";
import { BILLING_REFRESH_MS } from "#/features/billing/billing-model";
import { usePolling } from "#/hooks/use-polling";
import type { DashboardBillingAvailability } from "#/lib/dashboard/core/types.server";
import { loadBillingSummary } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

/**
 * Keeps a workspace's current-period summary fresh, refreshing about once a
 * minute. Mutations hand their returned summary to `replace`.
 */
export function useBillingSummary({
	workspaceId,
	initial,
	enabled = true,
}: {
	workspaceId: string;
	initial?: DashboardBillingAvailability;
	enabled?: boolean;
}) {
	const [billing, setBilling] = useState(initial);
	const [loadedAtMs, setLoadedAtMs] = useState(
		initial ? Date.now() : undefined,
	);
	const [error, setError] = useState<string>();
	const latest = useRef(0);

	const load = useCallback(async () => {
		const request = ++latest.current;
		try {
			const next = await loadBillingSummary({ data: { workspaceId } });
			if (request !== latest.current) return;
			setBilling(next);
			setLoadedAtMs(Date.now());
			setError(undefined);
		} catch (cause) {
			if (request === latest.current) setError(formatError(cause));
		}
	}, [workspaceId]);

	const skipInitialLoad = useRef(initial !== undefined);
	useEffect(() => {
		if (!enabled) return;
		if (skipInitialLoad.current) {
			skipInitialLoad.current = false;
			return;
		}
		void load();
	}, [enabled, load]);

	// Billing that is off on this installation will not turn on mid-session.
	usePolling(load, {
		enabled: enabled && billing?.available !== false,
		intervalMs: BILLING_REFRESH_MS,
	});

	const replace = useCallback((next: DashboardBillingAvailability) => {
		latest.current += 1;
		setBilling(next);
		setLoadedAtMs(Date.now());
		setError(undefined);
	}, []);

	return { billing, loadedAtMs, error, replace };
}
