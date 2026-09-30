import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useCreatedServiceCache } from "#/features/dashboard/state/created-service-cache";
import {
	applyLoaderState,
	applyMutationRecord,
	applyMutationStatus,
	applyServiceStatusSnapshot,
	applyServicesSnapshot,
	createNormalizedState,
	selectSelectedStatus,
	selectServicesArray,
} from "#/features/dashboard/state/dashboard-services";
import {
	parseServicesSnapshot,
	parseStatusSnapshot,
} from "#/features/dashboard/state/dashboard-snapshots";
import type {
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";
import { fetchGitHubCatalog } from "#/lib/dashboard/server-functions";
export function useDashboardState({
	state,
	urlSelectedServiceId,
}: {
	state: DashboardHomeState;
	urlSelectedServiceId?: string | null;
}) {
	const createdCache = useCreatedServiceCache();
	const [view, setView] = useState(() =>
		createNormalizedState(state, {
			selectedServiceId: urlSelectedServiceId ?? undefined,
		}),
	);
	const selectedId = view.selectedServiceId;
	const prevUrlSelectionRef = useRef(urlSelectedServiceId);
	useEffect(() => {
		if (prevUrlSelectionRef.current === urlSelectedServiceId) return;
		prevUrlSelectionRef.current = urlSelectedServiceId;
		if (urlSelectedServiceId === undefined) return;
		setView((current) =>
			current.selectedServiceId === urlSelectedServiceId
				? current
				: { ...current, selectedServiceId: urlSelectedServiceId },
		);
	}, [urlSelectedServiceId]);
	const environmentId = view.base.environment?.id ?? null;
	const services = useMemo(() => selectServicesArray(view), [view]);
	const [githubCatalogLoading, setGitHubCatalogLoading] = useState(false);
	const [githubCatalogLoaded, setGitHubCatalogLoaded] = useState(
		state.repositories.length > 0,
	);
	const githubCatalogPromiseRef = useRef<Promise<void> | null>(null);
	const selected = selectedId ? view.servicesById[selectedId] : undefined;
	const liveStatus = selectedId ? selectSelectedStatus(view) : null;
	const homeState: DashboardHomeState = useMemo(
		() => ({
			...view.base,
			services,
			servicesRevision: view.servicesRevision,
			selectedServiceId: selectedId,
		}),
		[view.base, services, view.servicesRevision, selectedId],
	);
	const ensureGitHubCatalog = useCallback(async () => {
		if (githubCatalogLoaded) return;
		if (githubCatalogPromiseRef.current) return githubCatalogPromiseRef.current;
		setGitHubCatalogLoading(true);
		const promise = fetchGitHubCatalog()
			.then((catalog) => {
				setView((current) => ({
					...current,
					base: {
						...current.base,
						githubAccount: catalog.githubAccount ?? current.base.githubAccount,
						repositories: catalog.repositories,
					},
				}));
				setGitHubCatalogLoaded(true);
			})
			.catch(() => {
				setView((current) => ({
					...current,
					base: { ...current.base, repositories: [] },
				}));
				setGitHubCatalogLoaded(true);
			})
			.finally(() => {
				githubCatalogPromiseRef.current = null;
				setGitHubCatalogLoading(false);
			});
		githubCatalogPromiseRef.current = promise;
		return promise;
	}, [githubCatalogLoaded]);
	const mergeStatusService = useCallback(
		(status: DashboardServiceStatus, basisRevision: string) => {
			setView((current) => applyMutationStatus(current, status, basisRevision));
		},
		[],
	);
	const mergeServiceRecord = useCallback(
		(service: DashboardServiceRecord, basisRevision: string) => {
			setView((current) =>
				applyMutationRecord(current, service, basisRevision),
			);
		},
		[],
	);
	const mergeEnvironmentServices = useCallback(
		(snapshot: {
			services: Array<DashboardServiceRecord>;
			revision: string;
		}) => {
			setView((current) => applyServicesSnapshot(current, snapshot));
		},
		[],
	);
	useEffect(() => {
		setView((current) =>
			applyLoaderState(
				current,
				{
					...state,
					githubAccount:
						githubCatalogLoaded || githubCatalogLoading
							? (current.base.githubAccount ?? state.githubAccount)
							: state.githubAccount,
					repositories:
						githubCatalogLoaded || githubCatalogLoading
							? current.base.repositories
							: state.repositories,
				},
				createdCache?.retainedServices(state.environment?.id ?? null),
			),
		);
	}, [state, githubCatalogLoaded, githubCatalogLoading, createdCache]);
	useEffect(() => {
		if (!environmentId) return;
		let active = true;
		const source = new EventSource(
			`/events/environment-services?environmentId=${encodeURIComponent(environmentId)}`,
		);
		source.addEventListener("services", (event) => {
			if (!active) return;
			mergeEnvironmentServices(
				parseServicesSnapshot(JSON.parse((event as MessageEvent<string>).data)),
			);
		});
		return () => {
			active = false;
			source.close();
		};
	}, [environmentId, mergeEnvironmentServices]);
	useEffect(() => {
		if (!selectedId) {
			return;
		}
		let active = true;
		const source = new EventSource(
			`/events/service-status?serviceId=${encodeURIComponent(selectedId)}`,
		);
		source.addEventListener("status", (event) => {
			if (!active) return;
			const snapshot = parseStatusSnapshot(
				JSON.parse((event as MessageEvent<string>).data),
			);
			setView((current) => applyServiceStatusSnapshot(current, snapshot));
		});
		return () => {
			active = false;
			source.close();
		};
	}, [selectedId]);
	return {
		createdCache,
		view,
		setView,
		selectedId,
		environmentId,
		services,
		githubCatalogLoading,
		selected,
		liveStatus,
		homeState,
		mergeStatusService,
		mergeServiceRecord,
		ensureGitHubCatalog,
	};
}
