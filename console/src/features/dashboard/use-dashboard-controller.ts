import { useRouter } from "@tanstack/react-router";
import {
	useCallback,
	useEffect,
	useMemo,
	useRef,
	useState,
	useTransition,
} from "react";
import { serviceLayoutPositions } from "#/features/dashboard/canvas/canvas-math";
import {
	nextNodePositionNear,
	nodePosition,
} from "#/features/dashboard/canvas/layout";
import { useDashboardCanvas } from "#/features/dashboard/canvas/use-dashboard-canvas";
import {
	type PendingServiceCreation,
	pendingServiceRecord,
} from "#/features/dashboard/services/pending-service";
import type { DashboardTab } from "#/features/dashboard/shared/types";
import {
	removeService,
	removeVolume,
	upsertServiceRecord,
	upsertVolume,
} from "#/features/dashboard/state/dashboard-services";
import type {
	CreateServiceFastResult,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { doSaveServicePosition } from "#/lib/dashboard/server-functions";
import { useEnvironmentRelease } from "./changes/use-environment-release";
import { useDashboardState } from "./state/use-dashboard-state";
export function useDashboardController({
	state,
	urlSelectedServiceId,
}: {
	state: DashboardHomeState;
	urlSelectedServiceId?: string | null;
}) {
	const router = useRouter();
	const {
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
	} = useDashboardState({ state, urlSelectedServiceId });

	const [pendingCreations, setPendingCreations] = useState<
		Array<PendingServiceCreation>
	>([]);
	const [selectedCreationId, setSelectedCreationId] = useState<string>();
	const [createError, setCreateError] = useState<string>();

	const pendingRecords = useMemo(
		() =>
			pendingCreations.map((entry) =>
				pendingServiceRecord(entry, environmentId),
			),
		[pendingCreations, environmentId],
	);
	const canvasServices = useMemo(
		() => [...services, ...pendingRecords],
		[services, pendingRecords],
	);
	const pendingServiceIds = useMemo(
		() => new Set(pendingRecords.map((service) => service.id)),
		[pendingRecords],
	);
	const [activeTab, setActiveTab] = useState<DashboardTab>("deployments");
	const [showNewService, setShowNewService] = useState(false);
	const [showEnvironmentDialog, setShowEnvironmentDialog] = useState(false);
	const [selectedVolumeId, setSelectedVolumeId] = useState<string | null>(null);
	const [showNewVolume, setShowNewVolume] = useState(false);
	const [mountVolumeId, setMountVolumeId] = useState<string | null>(null);
	const volumes = homeState.volumes;
	const stagedVolumeList = useMemo(
		() => volumes.filter((volume) => volume.staged),
		[volumes],
	);
	const selectedVolume = selectedVolumeId
		? volumes.find((volume) => volume.id === selectedVolumeId)
		: undefined;
	const mountVolume = mountVolumeId
		? volumes.find((volume) => volume.id === mountVolumeId)
		: undefined;

	const [, startTransition] = useTransition();
	const servicesRef = useRef(services);
	servicesRef.current = services;
	const revisionRef = useRef(view.servicesRevision);
	revisionRef.current = view.servicesRevision;
	const pendingCreationsRef = useRef(pendingCreations);
	pendingCreationsRef.current = pendingCreations;
	const creationCounterRef = useRef(0);
	const previousEnvironmentIdRef = useRef<string | null>(environmentId);

	const selectedPending = selectedCreationId
		? pendingRecords.find((service) => service.id === selectedCreationId)
		: undefined;
	const canvasSelectedId = selectedPending?.id ?? selectedId;
	const canvas = useDashboardCanvas({
		services: canvasServices,
		environmentId,
		selectedId: canvasSelectedId,
		selected: selected ?? selectedPending,
		onDeselect: () => {
			clearSelection();
		},
	});
	const { setNodePositions, nodePositionsRef } = canvas;

	const {
		discardingChangeCount,
		visibleServices,
		dirtyServices,
		changeSignature,
		totalUnappliedChanges,
		applyingChangeCount,
		deployableUnappliedChanges,
		hasPendingSpecWrites,
		showPrompt,
		deployingChanges,
		deployError,
		showChangeDetails,
		setShowChangeDetails,
		discardingChangeId,
		setSpecWriteState,
		handleDeployChanges,
		handleDiscardServiceChanges,
		handleDiscardChange,
		stagedVolumes,
		handleDiscardVolume,
	} = useEnvironmentRelease({
		services,
		stagedVolumes: stagedVolumeList,
		pendingCreationCount: pendingCreations.length,
		environmentId,
		revision: view.servicesRevision,
		mergeStatusService: (...args) => mergeStatusService(...args),
		mergeServiceRecord: (...args) => mergeServiceRecord(...args),
		handleServiceDeleted: (id) => handleServiceDeleted(id),
		handleVolumesDiscarded: (ids) => {
			for (const id of ids) handleVolumeDeleted(id);
		},
		onRefresh: () => handleRefresh(),
	});
	const navigateSelection = useCallback(
		(nextId: string | null) => {
			void router.navigate({
				to: ".",
				search: nextId ? { serviceId: nextId } : {},
				replace: true,
				resetScroll: false,
			});
		},
		[router],
	);

	const selectService = useCallback(
		(serviceId: string) => {
			setSelectedVolumeId(null);
			if (serviceId.startsWith("pending-")) {
				setSelectedCreationId(serviceId);
				setView((current) => ({ ...current, selectedServiceId: null }));
				setActiveTab("deployments");
				navigateSelection(null);
				return;
			}
			setSelectedCreationId(undefined);
			setView((current) =>
				current.selectedServiceId === serviceId
					? current
					: { ...current, selectedServiceId: serviceId },
			);
			setActiveTab("deployments");
			navigateSelection(serviceId);
		},
		[navigateSelection, setView],
	);

	const clearSelection = useCallback(() => {
		setSelectedVolumeId(null);
		setSelectedCreationId(undefined);
		setView((current) =>
			current.selectedServiceId === null
				? current
				: { ...current, selectedServiceId: null },
		);
		navigateSelection(null);
	}, [navigateSelection, setView]);

	const selectVolume = useCallback(
		(volumeId: string) => {
			clearSelection();
			setSelectedVolumeId(volumeId);
		},
		[clearSelection],
	);

	const setSelectedServiceWriteState = useCallback(
		(key: string, saving: boolean) => {
			if (selectedId) setSpecWriteState(selectedId, key, saving);
		},
		[selectedId, setSpecWriteState],
	);

	useEffect(() => {
		if (previousEnvironmentIdRef.current === environmentId) return;
		previousEnvironmentIdRef.current = environmentId;
		setSelectedVolumeId(null);
		setMountVolumeId(null);
		setShowNewVolume(false);
		setNodePositions(serviceLayoutPositions(servicesRef.current));
		nodePositionsRef.current = serviceLayoutPositions(servicesRef.current);
	}, [environmentId, nodePositionsRef, setNodePositions]);

	useEffect(() => {
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key !== "Escape" || event.defaultPrevented) return;
			if ((event.target as Element | null)?.closest?.('[role="dialog"]')) {
				return;
			}
			if (showNewService) {
				setShowNewService(false);
				setCreateError(undefined);
				return;
			}
			if (showChangeDetails) {
				setShowChangeDetails(false);
				return;
			}
			if (selectedId || selectedVolumeId) {
				clearSelection();
			}
		};
		document.addEventListener("keydown", onKeyDown);
		return () => document.removeEventListener("keydown", onKeyDown);
	}, [
		clearSelection,
		selectedId,
		selectedVolumeId,
		showChangeDetails,
		showNewService,
		setShowChangeDetails,
	]);

	const handleRefresh = () => {
		startTransition(() => {
			void router.invalidate();
		});
	};

	const handleNavigateEnvironment = (nextEnvironmentId: string | null) => {
		setView((current) =>
			current.selectedServiceId === null
				? current
				: { ...current, selectedServiceId: null },
		);
		startTransition(() => {
			void router.navigate(
				nextEnvironmentId
					? {
							to: "/environments/$environmentId",
							params: { environmentId: nextEnvironmentId },
							search: {},
						}
					: { to: "/", search: {} },
			);
		});
	};

	const openNewService = () => {
		setCreateError(undefined);
		setShowNewService(true);
		void ensureGitHubCatalog();
	};

	const preloadNewService = () => {
		void ensureGitHubCatalog();
	};

	const closeNewService = () => {
		setShowNewService(false);
		setCreateError(undefined);
	};

	const handleTabChange = (tab: DashboardTab) => {
		setActiveTab(tab);
		if (tab === "settings") {
			void ensureGitHubCatalog();
		}
	};

	const mergeService = (service: DashboardServiceRecord) => {
		mergeServiceRecord(service, revisionRef.current);
	};

	const handleCreating = (selector: string) => {
		const trimmed = selector.trim();
		if (!trimmed) return;
		creationCounterRef.current += 1;
		const clientId = `${Date.now().toString(36)}-${creationCounterRef.current}`;
		const shortName = trimmed.split("/").pop()?.trim() || "New service";
		const spawnPosition = nextNodePositionNear(
			canvasServices.map(
				(service, index) =>
					canvas.servicePositions[service.id] ??
					service.layoutPosition ??
					nodePosition(index),
			),
		);
		const provisionalId = `pending-${clientId}`;
		canvas.setNodePositions((current) => ({
			...current,
			[provisionalId]: spawnPosition,
		}));
		canvas.nodePositionsRef.current = {
			...canvas.nodePositionsRef.current,
			[provisionalId]: spawnPosition,
		};
		setPendingCreations((current) => [
			...current,
			{ clientId, selector: trimmed, name: shortName, position: spawnPosition },
		]);
		setSelectedCreationId(provisionalId);
		setCreateError(undefined);
		setShowNewService(false);
	};

	const handleCreateFailed = (selector: string, message: string) => {
		const normalized = selector.trim().toLowerCase();
		const failed = pendingCreationsRef.current.find(
			(entry) => entry.selector.trim().toLowerCase() === normalized,
		);
		setPendingCreations((current) =>
			current.filter((entry) => entry.clientId !== failed?.clientId),
		);
		if (failed) {
			setSelectedCreationId((current) =>
				current === `pending-${failed.clientId}` ? undefined : current,
			);
		}
		setCreateError(message);
		setShowNewService(true);
	};

	const handleCreated = (result: CreateServiceFastResult) => {
		const createdSelector =
			result.service.spec?.source?.sourceSpec?.repositorySelector
				?.trim()
				.toLowerCase();
		const matchingPending = createdSelector
			? pendingCreationsRef.current.find(
					(entry) => entry.selector.trim().toLowerCase() === createdSelector,
				)
			: undefined;
		if (matchingPending) {
			setPendingCreations((current) =>
				current.filter((entry) => entry.clientId !== matchingPending.clientId),
			);
		}
		const existingService = services.some(
			(service) => service.id === result.service.id,
		);
		const spawnPosition =
			result.service.layoutPosition ??
			matchingPending?.position ??
			nextNodePositionNear(
				services.map(
					(service, index) =>
						canvas.servicePositions[service.id] ??
						service.layoutPosition ??
						nodePosition(index),
				),
			);

		if (!existingService && !result.service.layoutPosition) {
			const provisionalId = matchingPending
				? `pending-${matchingPending.clientId}`
				: undefined;
			canvas.setNodePositions((current) => {
				const next = { ...current, [result.service.id]: spawnPosition };
				if (provisionalId) delete next[provisionalId];
				return next;
			});
			const positions = {
				...canvas.nodePositionsRef.current,
				[result.service.id]: spawnPosition,
			};
			if (provisionalId) delete positions[provisionalId];
			canvas.nodePositionsRef.current = positions;
			if (result.environment.id) {
				void doSaveServicePosition({
					data: {
						environmentId: result.environment.id,
						serviceId: result.service.id,
						position: spawnPosition,
					},
				});
			}
		}

		createdCache?.remember(result.service, result.environment);
		previousEnvironmentIdRef.current = result.environment.id ?? null;

		setView((current) => {
			const next = upsertServiceRecord(current, result.service);
			const nextEnvironments = current.base.environments.some(
				(entry) => entry.id === result.environment.id,
			)
				? current.base.environments
				: [...current.base.environments, result.environment];
			return {
				...next,
				base: {
					...current.base,
					project:
						current.base.project?.id === result.project.id
							? current.base.project
							: result.project,
					environment: result.environment,
					environments: nextEnvironments,
					onboarding: result.onboarding,
					controlPlaneReachable: true,
					controlPlaneError: undefined,
				},
				selectedServiceId: result.service.id,
			};
		});
		setSelectedCreationId(undefined);
		canvas.hasUserPanned.current = false;
		setActiveTab("deployments");
		setShowNewService(false);
		if (!environmentId || environmentId !== result.environment.id) {
			startTransition(() => {
				void router.navigate({
					to: "/environments/$environmentId",
					params: { environmentId: result.environment.id },
					search: { serviceId: result.service.id },
				});
			});
		} else {
			navigateSelection(result.service.id);
			startTransition(() => void router.invalidate());
		}
		if (result.serviceStatus) {
			mergeStatusService(result.serviceStatus, revisionRef.current);
		}
	};

	const handleVolumeUpserted = (volume: DashboardVolume) => {
		setView((current) => upsertVolume(current, volume));
	};

	const handleVolumeDeleted = (volumeId: string) => {
		setView((current) => removeVolume(current, volumeId));
		setSelectedVolumeId((current) => (current === volumeId ? null : current));
		startTransition(() => void router.invalidate());
	};

	const handleServiceDeleted = (serviceId: string) => {
		createdCache?.forget(serviceId);
		setView((current) => removeService(current, serviceId));
		if (selectedId === serviceId) {
			navigateSelection(null);
		}
		startTransition(() => void router.invalidate());
	};

	return {
		homeState,
		canvas: {
			...canvas,
			services: [...visibleServices, ...pendingRecords],
			pendingServiceIds,
			selectedId: canvasSelectedId,
			volumes,
			selectedVolumeId: selectedVolume?.id ?? null,
		},
		navigation: {
			environmentDialogOpen: showEnvironmentDialog,
			openEnvironmentDialog: () => setShowEnvironmentDialog(true),
			closeEnvironmentDialog: () => setShowEnvironmentDialog(false),
			navigateEnvironment: handleNavigateEnvironment,
			refresh: handleRefresh,
		},
		selection: {
			service: selected,
			pending: selectedPending,
			status: liveStatus,
			project: view.base.project,
			tab: activeTab,
			changeTab: handleTabChange,
			clear: clearSelection,
			select: selectService,
			update: mergeService,
			delete: handleServiceDeleted,
			reportSaving: setSelectedServiceWriteState,
		},
		volumes: {
			all: volumes,
			services,
			environmentId,
			selected: selectedVolume,
			select: selectVolume,
			createOpen: showNewVolume,
			openCreate: () => setShowNewVolume(true),
			closeCreate: () => setShowNewVolume(false),
			mountTarget: mountVolume,
			openMount: (volumeId: string) => setMountVolumeId(volumeId),
			closeMount: () => setMountVolumeId(null),
			upsert: handleVolumeUpserted,
			remove: handleVolumeDeleted,
			serviceUpdated: mergeService,
		},
		creation: {
			isOpen: showNewService,
			open: openNewService,
			preload: preloadNewService,
			close: closeNewService,
			error: createError,
			catalogLoading: githubCatalogLoading,
			onCreated: handleCreated,
			onCreating: handleCreating,
			onFailed: handleCreateFailed,
		},
		release: {
			discardingChangeCount,
			pendingCreationCount: pendingCreations.length,
			showPrompt,
			changeSignature,
			deployError,
			applyingChangeCount,
			deployingChanges,
			deployableUnappliedChanges,
			hasPendingSpecWrites,
			totalUnappliedChanges,
			setShowChangeDetails,
			handleDeployChanges,
			showChangeDetails,
			dirtyServices,
			discardingChangeId,
			handleDiscardChange,
			handleDiscardServiceChanges,
			stagedVolumes,
			handleDiscardVolume,
		},
	};
}
