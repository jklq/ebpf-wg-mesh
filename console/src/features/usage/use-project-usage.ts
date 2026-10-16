import { useCallback, useEffect, useRef, useState } from "react";
import { type UsageRange, usageWindow } from "#/features/usage/usage-model";
import { usePolling } from "#/hooks/use-polling";
import type { DashboardProjectUsage } from "#/lib/dashboard/core/types.server";
import { loadProjectUsage } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

const REFRESH_MS = 60_000;

/** Loads a range of project usage and refreshes it every minute unless paused. */
export function useProjectUsage({
	projectId,
	serviceId,
	range,
	initial,
	paused = false,
}: {
	projectId: string;
	serviceId?: string;
	range: UsageRange;
	/** Usage the route already loaded for the initial range. */
	initial?: { usage: DashboardProjectUsage; loadedAtMs: number };
	paused?: boolean;
}) {
	const [usage, setUsage] = useState(initial?.usage);
	const [loadedAtMs, setLoadedAtMs] = useState(initial?.loadedAtMs);
	const [loading, setLoading] = useState(!initial);
	const [error, setError] = useState<string>();
	const latest = useRef(0);
	const skipInitialLoad = useRef(initial !== undefined);

	const load = useCallback(async () => {
		const request = ++latest.current;
		setLoading(true);
		try {
			const next = await loadProjectUsage({
				data: { projectId, serviceId, ...usageWindow(range, Date.now()) },
			});
			if (request !== latest.current) return;
			setUsage(next);
			setLoadedAtMs(Date.now());
			setError(undefined);
		} catch (cause) {
			if (request === latest.current) setError(formatError(cause));
		} finally {
			if (request === latest.current) setLoading(false);
		}
	}, [projectId, serviceId, range]);

	useEffect(() => {
		if (skipInitialLoad.current) {
			skipInitialLoad.current = false;
			return;
		}
		void load();
	}, [load]);

	usePolling(load, { enabled: !paused, intervalMs: REFRESH_MS });

	return { usage, loadedAtMs, loading, error };
}
