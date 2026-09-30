import {
	createContext,
	type ReactNode,
	useCallback,
	useContext,
	useMemo,
	useState,
} from "react";

import type {
	DashboardEnvironment,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

export interface CreatedServiceEntry {
	service: DashboardServiceRecord;
	environment: DashboardEnvironment;
}

interface CreatedServiceCache {
	remember: (
		service: DashboardServiceRecord,
		environment: DashboardEnvironment,
	) => void;
	forget: (serviceId: string) => void;
	retainedServices: (
		environmentId: string | null,
	) => Array<DashboardServiceRecord>;
	retainedEnvironment: (
		environmentId: string,
	) => DashboardEnvironment | undefined;
}

const CreatedServiceCacheContext = createContext<CreatedServiceCache | null>(
	null,
);

export function CreatedServiceCacheProvider({
	children,
}: {
	children: ReactNode;
}) {
	const [entries, setEntries] = useState<Record<string, CreatedServiceEntry>>(
		{},
	);

	const remember = useCallback(
		(service: DashboardServiceRecord, environment: DashboardEnvironment) => {
			setEntries((current) =>
				current[service.id]?.service === service &&
				current[service.id]?.environment === environment
					? current
					: { ...current, [service.id]: { service, environment } },
			);
		},
		[],
	);

	const forget = useCallback((serviceId: string) => {
		setEntries((current) => {
			if (!current[serviceId]) return current;
			const next = { ...current };
			delete next[serviceId];
			return next;
		});
	}, []);

	const value = useMemo<CreatedServiceCache>(
		() => ({
			remember,
			forget,
			retainedServices: (environmentId: string | null) =>
				Object.values(entries)
					.filter((entry) =>
						environmentId
							? entry.service.environmentId === environmentId
							: true,
					)
					.map((entry) => entry.service),
			retainedEnvironment: (environmentId: string) =>
				Object.values(entries).find(
					(entry) => entry.environment.id === environmentId,
				)?.environment,
		}),
		[entries, forget, remember],
	);

	return (
		<CreatedServiceCacheContext.Provider value={value}>
			{children}
		</CreatedServiceCacheContext.Provider>
	);
}

export function useCreatedServiceCache(): CreatedServiceCache | null {
	return useContext(CreatedServiceCacheContext);
}
