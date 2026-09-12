import { useRouter } from "@tanstack/react-router";
import { Loader2, X } from "lucide-react";
import {
	lazy,
	Suspense,
	useCallback,
	useEffect,
	useMemo,
	useRef,
	useState,
	useTransition,
} from "react";
import { cn } from "#/lib/cn";
import {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "#/lib/dashboard/core/defaults";
import type {
	CreateServiceFastResult,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";
import { panelIconBtn } from "#/lib/ui-classes";

import { useCreatedServiceCache } from "./created-service-cache";
import {
	DashboardCanvasStage,
	ServicePanelFallback,
	serviceLayoutPositions,
} from "./dashboard-canvas";
import {
	hydrateServicesSnapshot,
	hydrateStatusSnapshot,
} from "./dashboard-hydrate";
import {
	applyLoaderState,
	applyMutationRecord,
	applyMutationStatus,
	applyServiceStatusSnapshot,
	applyServicesSnapshot,
	createNormalizedState,
	removeService,
	selectSelectedStatus,
	selectServicesArray,
	upsertServiceRecord,
} from "./dashboard-services";
import {
	type ApplyingServiceChanges,
	applyingChangeKey,
	hasUnappliedChanges,
	mergeApplyingServices,
	queuedChangeCount,
	snapshotApplyingChanges,
	UnappliedChangesDialog,
	unappliedChangeCount,
} from "./dashboard-unapplied";
import { EnvironmentDialog } from "./environment-dialog";
import { nextNodePositionNear, nodePosition } from "./layout";
import { NewServiceModal } from "./new-service-modal";
import {
	doDeleteService,
	doDiscardServiceChanges,
	doReleaseEnvironment,
	doSaveServicePosition,
	fetchGitHubCatalog,
} from "./server-fns";
import { formatError } from "./service-utils";
import { Topbar } from "./topbar";
import type { DashboardTab } from "./types";
import { useDashboardCanvas } from "./use-dashboard-canvas";

export { DashboardCanvasSkeleton } from "./dashboard-canvas";

interface PendingServiceCreation {
	clientId: string;
	selector: string;
	name: string;
	position: { x: number; y: number };
}

function pendingServiceRecord(
	entry: PendingServiceCreation,
	environmentId: string | null,
): DashboardServiceRecord {
	const id = `pending-${entry.clientId}`;
	const now = new Date();
	return {
		id,
		environmentId: environmentId ?? "",
		name: entry.name,
		spec: {
			source: {
				provider: "github",
				repositorySelector: entry.selector,
				trackedRef: "main",
				buildRecipe: { dockerfilePath: "", contextDir: "." },
			},
			runtime: {
				env: {},
				cpuMillis: DEFAULT_SERVICE_CPU_MILLIS,
				memoryMebibytes: DEFAULT_SERVICE_MEMORY_MEBIBYTES,
				ports: [],
			},
		},
		createdAt: now,
		updatedAt: now,
	};
}

const ServicePanel = lazy(() =>
	import("./service-panel").then((module) => ({
		default: module.ServicePanel,
	})),
);

function PendingServicePanel({
	service,
	onClose,
}: {
	service: DashboardServiceRecord;
	onClose: () => void;
}) {
	return (
		<>
			<div className="flex h-header shrink-0 items-center gap-1.5 border-b border-line px-4 py-2">
				<strong className="overflow-hidden font-display text-[22px] font-medium tracking-[-0.03em] text-ellipsis whitespace-nowrap text-ink">
					{service.name}
				</strong>
				<span className="flex-1" />
				<button
					type="button"
					className={panelIconBtn}
					onClick={onClose}
					title="Close service panel"
				>
					<X size={14} />
				</button>
			</div>
			<div className="flex min-h-0 flex-1 items-center justify-center p-8">
				<div className="flex max-w-72 flex-col items-center text-center">
					<Loader2 size={22} className="animate-spin text-building" />
					<div className="mt-4 font-display text-lg font-medium text-ink">
						Creating service
					</div>
					<div className="mt-1 font-mono text-xs text-muted">
						{service.spec?.source?.repositorySelector}
					</div>
					<div className="mt-3 text-xs leading-relaxed text-dim">
						Preparing the service and its first undeployed configuration.
					</div>
				</div>
			</div>
		</>
	);
}

export function DashboardPage({
	state,
	urlSelectedServiceId,
}: {
	state: DashboardHomeState;
	urlSelectedServiceId?: string | null;
}) {
	const router = useRouter();
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
	const [pendingCreations, setPendingCreations] = useState<
		Array<PendingServiceCreation>
	>([]);
	const [selectedCreationId, setSelectedCreationId] = useState<string>();
	const [createError, setCreateError] = useState<string>();
	const services = useMemo(() => selectServicesArray(view), [view]);
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
	const [githubCatalogLoading, setGitHubCatalogLoading] = useState(false);
	const [githubCatalogLoaded, setGitHubCatalogLoaded] = useState(
		state.repositories.length > 0,
	);
	const [deployingChanges, setDeployingChanges] = useState(false);
	const [applyingServices, setApplyingServices] = useState<
		Array<ApplyingServiceChanges>
	>([]);
	const [deployError, setDeployError] = useState<string>();
	const [showChangeDetails, setShowChangeDetails] = useState(false);
	const [discardingChangeId, setDiscardingChangeId] = useState<string>();
	const [pendingSpecWrites, setPendingSpecWrites] = useState<Set<string>>(
		() => new Set(),
	);
	const [, startTransition] = useTransition();
	const servicesRef = useRef(services);
	servicesRef.current = services;
	const revisionRef = useRef(view.servicesRevision);
	revisionRef.current = view.servicesRevision;
	const environmentIdRef = useRef(environmentId);
	environmentIdRef.current = environmentId;
	const pendingSpecWritesRef = useRef(pendingSpecWrites);
	pendingSpecWritesRef.current = pendingSpecWrites;
	const pendingCreationsRef = useRef(pendingCreations);
	pendingCreationsRef.current = pendingCreations;
	const creationCounterRef = useRef(0);
	const specWriteWaitersRef = useRef<Array<() => void>>([]);
	const previousEnvironmentIdRef = useRef<string | null>(environmentId);
	const githubCatalogPromiseRef = useRef<Promise<void> | null>(null);
	const selected = selectedId ? view.servicesById[selectedId] : undefined;
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
	const dirtyServices = services.filter(hasUnappliedChanges);
	const changeSignature = dirtyServices
		.flatMap((service) =>
			(service.unappliedChanges ?? []).map(
				(change) =>
					`${service.id}:${change.id}:${change.action}:${change.newValue}`,
			),
		)
		.sort()
		.join("|");
	const totalUnappliedChanges = dirtyServices.reduce(
		(total, service) => total + unappliedChangeCount(service),
		0,
	);
	const applyingChangeKeys = useMemo(
		() =>
			new Set(
				applyingServices.flatMap((service) =>
					service.changeIds.map((changeId) =>
						applyingChangeKey(service.serviceId, changeId),
					),
				),
			),
		[applyingServices],
	);
	const applyingChangeCount = applyingServices.reduce(
		(total, service) => total + service.count,
		0,
	);
	const deployableUnappliedChanges = dirtyServices.reduce(
		(total, service) => total + queuedChangeCount(service, applyingChangeKeys),
		0,
	);
	const hasPendingSpecWrites = pendingSpecWrites.size > 0;
	const showPrompt =
		deployableUnappliedChanges > 0 ||
		applyingChangeCount > 0 ||
		hasPendingSpecWrites ||
		Boolean(deployError);

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
		[navigateSelection],
	);

	const clearSelection = useCallback(() => {
		setSelectedCreationId(undefined);
		setView((current) =>
			current.selectedServiceId === null
				? current
				: { ...current, selectedServiceId: null },
		);
		navigateSelection(null);
	}, [navigateSelection]);

	const setSpecWriteState = useCallback(
		(serviceId: string, key: string, saving: boolean) => {
			const writeKey = `${serviceId}:${key}`;
			setPendingSpecWrites((current) => {
				if (current.has(writeKey) === saving) return current;
				const next = new Set(current);
				if (saving) next.add(writeKey);
				else next.delete(writeKey);
				return next;
			});
		},
		[],
	);
	const setSelectedServiceWriteState = useCallback(
		(key: string, saving: boolean) => {
			if (selectedId) setSpecWriteState(selectedId, key, saving);
		},
		[selectedId, setSpecWriteState],
	);

	useEffect(() => {
		if (hasPendingSpecWrites) return;
		const waiters = specWriteWaitersRef.current.splice(0);
		for (const resolve of waiters) resolve();
	}, [hasPendingSpecWrites]);

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

	useEffect(() => {
		if (previousEnvironmentIdRef.current === environmentId) return;
		previousEnvironmentIdRef.current = environmentId;
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
			if (selectedId) {
				clearSelection();
			}
		};
		document.addEventListener("keydown", onKeyDown);
		return () => document.removeEventListener("keydown", onKeyDown);
	}, [clearSelection, selectedId, showChangeDetails, showNewService]);

	const mergeStatusService = useCallback(
		(status: DashboardServiceStatus, basisRevision: number) => {
			setView((current) => applyMutationStatus(current, status, basisRevision));
		},
		[],
	);

	const mergeServiceRecord = useCallback(
		(service: DashboardServiceRecord, basisRevision: number) => {
			setView((current) =>
				applyMutationRecord(current, service, basisRevision),
			);
		},
		[],
	);

	const mergeEnvironmentServices = useCallback(
		(snapshot: {
			services: Array<DashboardServiceRecord>;
			revision: number;
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
		if (totalUnappliedChanges === 0) {
			setDeployError(undefined);
		}
	}, [totalUnappliedChanges]);

	useEffect(() => {
		if (!environmentId) return;
		let active = true;
		const source = new EventSource(
			`/events/environment-services?environmentId=${encodeURIComponent(environmentId)}`,
		);
		source.addEventListener("services", (event) => {
			if (!active) return;
			mergeEnvironmentServices(
				hydrateServicesSnapshot(
					JSON.parse((event as MessageEvent<string>).data),
				),
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
			const snapshot = hydrateStatusSnapshot(
				JSON.parse((event as MessageEvent<string>).data),
			);
			setView((current) => applyServiceStatusSnapshot(current, snapshot));
		});
		return () => {
			active = false;
			source.close();
		};
	}, [selectedId]);

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
		setView((current) => upsertServiceRecord(current, service));
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
			current.filter(
				(entry) => entry.selector.trim().toLowerCase() !== normalized,
			),
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
		const createdSelector = result.service.spec?.source?.repositorySelector
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

	const handleServiceDeleted = (serviceId: string) => {
		createdCache?.forget(serviceId);
		setView((current) => removeService(current, serviceId));
		if (selectedId === serviceId) {
			navigateSelection(null);
		}
		startTransition(() => void router.invalidate());
	};

	const deployServiceBatch = async (
		servicesToDeploy: Array<DashboardServiceRecord>,
		currentEnvironmentId: string,
	) => {
		const applying = servicesToDeploy.map(snapshotApplyingChanges);
		const basisRevision = revisionRef.current;
		setDeployError(undefined);
		setShowChangeDetails(false);
		setDeployingChanges(true);
		setApplyingServices((current) => mergeApplyingServices(current, applying));
		try {
			const statuses = await doReleaseEnvironment({
				data: { environmentId: currentEnvironmentId },
			});
			for (const status of statuses) {
				mergeStatusService(status, basisRevision);
			}
			startTransition(() => void router.invalidate());
		} catch (error) {
			setDeployError(`Deploy stopped: ${formatError(error)}`);
		} finally {
			setApplyingServices((current) =>
				current.filter(
					(service) =>
						!applying.some(
							(applied) => applied.serviceId === service.serviceId,
						),
				),
			);
			setDeployingChanges(false);
		}
	};

	const handleDeployChanges = async () => {
		if (deployingChanges) return;
		if (!environmentId) return;
		setDeployingChanges(true);
		if (pendingSpecWritesRef.current.size > 0) {
			await new Promise<void>((resolve) => {
				specWriteWaitersRef.current.push(resolve);
			});
		}
		const currentEnvironmentId = environmentIdRef.current;
		const currentServices = servicesRef.current.filter(hasUnappliedChanges);
		if (!currentEnvironmentId || currentServices.length === 0) {
			setDeployingChanges(false);
			return;
		}
		await deployServiceBatch(currentServices, currentEnvironmentId);
	};

	const handleDiscardServiceChanges = async (serviceId: string) => {
		if (discardingChangeId) return;
		setDiscardingChangeId(`service:${serviceId}`);
		const basisRevision = revisionRef.current;
		try {
			const currentService = servicesRef.current.find(
				(service) => service.id === serviceId,
			);
			const hasOtherUndeployedServices = servicesRef.current.some(
				(service) => service.id !== serviceId && hasUnappliedChanges(service),
			);
			const rolloutGeneration =
				currentService?.rolloutGeneration ??
				currentService?.latestDeployment?.rolloutGeneration ??
				0;
			if (currentService && rolloutGeneration === 0) {
				await doDeleteService({ data: { serviceId } });
				handleServiceDeleted(serviceId);
				if (!hasOtherUndeployedServices) setShowChangeDetails(false);
				return;
			}
			const service = await doDiscardServiceChanges({
				data: { serviceId, discardAll: true },
			});
			mergeServiceRecord(service, basisRevision);
			if (!hasOtherUndeployedServices && !hasUnappliedChanges(service)) {
				setShowChangeDetails(false);
			}
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
		} finally {
			setDiscardingChangeId(undefined);
		}
	};

	const handleDiscardChange = async (serviceId: string, changeId: string) => {
		if (discardingChangeId) return;
		setDiscardingChangeId(`${serviceId}:${changeId}`);
		const basisRevision = revisionRef.current;
		try {
			const service = await doDiscardServiceChanges({
				data: { serviceId, changeIds: [changeId] },
			});
			mergeServiceRecord(service, basisRevision);
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
		} finally {
			setDiscardingChangeId(undefined);
		}
	};

	return (
		<div className="flex h-dvh flex-col overflow-hidden">
			<Topbar
				state={homeState}
				onNewService={openNewService}
				onPreloadNewService={preloadNewService}
				onRefresh={handleRefresh}
				onNewEnvironment={() => setShowEnvironmentDialog(true)}
				onEnvironmentsChanged={handleRefresh}
				onNavigateEnvironment={handleNavigateEnvironment}
			/>
			{showEnvironmentDialog && (
				<EnvironmentDialog
					state={homeState}
					onClose={() => setShowEnvironmentDialog(false)}
					onCreated={(environmentId) => {
						setShowEnvironmentDialog(false);
						handleNavigateEnvironment(environmentId);
					}}
				/>
			)}
			<DashboardCanvasStage
				canvasRef={canvas.canvasRef}
				panOffset={canvas.panOffset}
				zoom={canvas.zoom}
				setZoom={canvas.setZoom}
				canvasReady={canvas.canvasReady}
				showCanvasSkeleton={canvas.showCanvasSkeleton}
				servicePositions={canvas.servicePositions}
				onCanvasMouseDown={canvas.onCanvasMouseDown}
				onServiceMouseDown={canvas.onServiceMouseDown}
				onCanvasClick={canvas.onCanvasClick}
				onSelectService={(serviceId) => {
					const id = canvas.selectService(serviceId);
					if (!id) return;
					selectService(id);
				}}
				onResetView={canvas.handleResetView}
				onEscape={() => {
					clearSelection();
				}}
				panCursor={Boolean(canvas.panStart.current)}
				panelOpen={Boolean(selected || selectedPending)}
				services={canvasServices}
				pendingServiceIds={pendingServiceIds}
				selectedId={canvasSelectedId}
				localState={homeState}
				showNewService={showNewService}
				onAddService={openNewService}
				onPreloadAdd={preloadNewService}
				showPrompt={showPrompt}
				changeSignature={changeSignature}
				deployError={deployError}
				applyingChangeCount={applyingChangeCount}
				deployingChanges={deployingChanges}
				deployableUnappliedChanges={deployableUnappliedChanges}
				hasPendingSpecWrites={hasPendingSpecWrites}
				totalUnappliedChanges={totalUnappliedChanges}
				onShowDetails={() => setShowChangeDetails(true)}
				onDeploy={handleDeployChanges}
			/>

			<div
				className={cn(
					"fixed top-0 right-0 z-50 flex h-screen w-side-panel max-w-full translate-x-full flex-col overflow-hidden border-l border-line bg-[radial-gradient(920px_420px_at_100%_-40px,rgba(226,138,36,0.08),transparent_58%),linear-gradient(180deg,#1d1a16_0%,#141210_72%)] transition-transform duration-200 ease-out max-[900px]:w-screen",
					(selected || selectedPending) && "translate-x-0",
				)}
			>
				<div className="panel-grain" />
				{selectedPending ? (
					<PendingServicePanel
						service={selectedPending}
						onClose={clearSelection}
					/>
				) : selected ? (
					<Suspense
						fallback={
							<ServicePanelFallback
								service={selected}
								onClose={clearSelection}
								onRefresh={handleRefresh}
							/>
						}
					>
						<ServicePanel
							service={selected}
							status={liveStatus}
							project={view.base.project}
							state={homeState}
							activeTab={activeTab}
							onTabChange={handleTabChange}
							onClose={() => {
								clearSelection();
							}}
							onRefresh={handleRefresh}
							onServiceUpdated={mergeService}
							onServiceDeleted={handleServiceDeleted}
							onSpecSaveStateChange={setSelectedServiceWriteState}
						/>
					</Suspense>
				) : null}
			</div>

			{showNewService && (
				<NewServiceModal
					state={homeState}
					catalogLoading={githubCatalogLoading}
					initialError={createError}
					onClose={closeNewService}
					onCreated={handleCreated}
					onCreating={handleCreating}
					onCreateFailed={handleCreateFailed}
				/>
			)}

			{showChangeDetails && (
				<UnappliedChangesDialog
					services={dirtyServices}
					totalChanges={totalUnappliedChanges}
					applyingChanges={applyingChangeCount}
					deployableChanges={deployableUnappliedChanges}
					affectedServices={dirtyServices.length}
					deploying={deployingChanges}
					deployError={deployError}
					discardingChangeId={discardingChangeId}
					onClose={() => setShowChangeDetails(false)}
					onDeploy={handleDeployChanges}
					onDiscardChange={handleDiscardChange}
					onDiscardService={handleDiscardServiceChanges}
				/>
			)}
		</div>
	);
}
