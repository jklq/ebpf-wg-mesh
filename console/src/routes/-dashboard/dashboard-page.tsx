import { useRouter } from "@tanstack/react-router";
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
import type {
	CreateServiceFastResult,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import {
	DashboardCanvasStage,
	ServicePanelFallback,
	serviceLayoutPositions,
} from "./dashboard-canvas";
import {
	hydrateServiceSnapshots,
	hydrateServiceStatusSnapshot,
} from "./dashboard-hydrate";
import {
	clearPendingCreatedService,
	hydrateStateWithPendingCreated,
	mergeHomeEnvironmentServices,
	mergeHomeService,
	mergeHomeStatusService,
	readPendingCreatedService,
	rememberPendingCreatedService,
	withPendingCreatedService,
} from "./dashboard-pending";
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
import {
	doDiscardServiceChanges,
	doReleaseEnvironment,
	doSaveServicePosition,
	fetchGitHubCatalog,
} from "./server-fns";
import { newestServiceRecord } from "./service-record-order";
import { formatError } from "./service-utils";
import { Topbar } from "./topbar";
import type { DashboardTab } from "./types";
import { useDashboardCanvas } from "./use-dashboard-canvas";

export { DashboardCanvasSkeleton } from "./dashboard-canvas";
export { resetDashboardPageTestState } from "./dashboard-pending";

const loadNewServiceModal = () =>
	import("./new-service-modal").then((module) => ({
		default: module.NewServiceModal,
	}));
const NewServiceModal = lazy(loadNewServiceModal);
const ServicePanel = lazy(() =>
	import("./service-panel").then((module) => ({
		default: module.ServicePanel,
	})),
);

export function DashboardPage({ state }: { state: DashboardHomeState }) {
	const router = useRouter();
	const [selectedId, setSelectedId] = useState<string | null>(
		() => readPendingCreatedService()?.service.id ?? null,
	);
	const [localState, setLocalState] = useState(() =>
		hydrateStateWithPendingCreated(state),
	);
	const services = localState.services;
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
	const environmentId = localState.environment?.id ?? null;
	const servicesRef = useRef(services);
	servicesRef.current = services;
	const environmentIdRef = useRef(environmentId);
	environmentIdRef.current = environmentId;
	const pendingSpecWritesRef = useRef(pendingSpecWrites);
	pendingSpecWritesRef.current = pendingSpecWrites;
	const specWriteWaitersRef = useRef<Array<() => void>>([]);
	const previousEnvironmentIdRef = useRef<string | null>(environmentId);
	const githubCatalogPromiseRef = useRef<Promise<void> | null>(null);
	const selected = services.find((service) => service.id === selectedId);
	const canvas = useDashboardCanvas({
		services,
		environmentId,
		selectedId,
		selected,
		onDeselect: () => {
			clearPendingCreatedService(selectedId ?? undefined);
			setSelectedId(null);
			setLocalState((current) => ({ ...current, serviceStatus: undefined }));
		},
	});
	const { setNodePositions, nodePositionsRef } = canvas;
	const liveStatus =
		selectedId && localState.serviceStatus?.service.id === selectedId
			? localState.serviceStatus
			: null;
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
				setLocalState((current) => ({
					...current,
					githubAccount: catalog.githubAccount ?? current.githubAccount,
					repositories: catalog.repositories,
				}));
				setGitHubCatalogLoaded(true);
			})
			.catch(() => {
				setLocalState((current) => ({ ...current, repositories: [] }));
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
		const pending = readPendingCreatedService();
		const keepCreatedSelection =
			Boolean(pending) && environmentId === (pending?.environment.id ?? null);
		previousEnvironmentIdRef.current = environmentId;
		if (keepCreatedSelection && pending) {
			setSelectedId(pending.service.id);
		} else {
			setSelectedId(null);
			setLocalState((current) => ({ ...current, serviceStatus: undefined }));
		}
		setNodePositions(serviceLayoutPositions(localState.services));
		nodePositionsRef.current = serviceLayoutPositions(localState.services);
	}, [environmentId, localState.services, nodePositionsRef, setNodePositions]);

	useEffect(() => {
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key !== "Escape" || event.defaultPrevented) return;
			if ((event.target as Element | null)?.closest?.('[role="dialog"]')) {
				return;
			}
			if (showNewService) {
				setShowNewService(false);
				return;
			}
			if (showChangeDetails) {
				setShowChangeDetails(false);
				return;
			}
			if (selectedId) {
				clearPendingCreatedService(selectedId);
				setSelectedId(null);
				setLocalState((current) => ({ ...current, serviceStatus: undefined }));
			}
		};
		document.addEventListener("keydown", onKeyDown);
		return () => document.removeEventListener("keydown", onKeyDown);
	}, [selectedId, showChangeDetails, showNewService]);

	const mergeStatusService = useCallback(
		(status: DashboardServiceStatus) => {
			setLocalState((current) =>
				mergeHomeStatusService(current, status, selectedId),
			);
		},
		[selectedId],
	);

	const mergeEnvironmentServices = useCallback(
		(nextServices: Array<DashboardServiceRecord>) => {
			setLocalState((current) =>
				mergeHomeEnvironmentServices(current, nextServices),
			);
		},
		[],
	);

	useEffect(() => {
		const pending = readPendingCreatedService();
		const pendingEnvironment =
			!state.environment && pending ? pending.environment : undefined;
		const pendingEnvironments = pendingEnvironment
			? state.environments.some((entry) => entry.id === pendingEnvironment.id)
				? state.environments
				: [...state.environments, pendingEnvironment]
			: state.environments;
		setLocalState((current) => {
			const services = withPendingCreatedService(state.services).map(
				(service) =>
					newestServiceRecord(
						current.services.find((entry) => entry.id === service.id),
						service,
					),
			);
			const selectedService = current.service
				? services.find((service) => service.id === current.service?.id)
				: state.service;
			const statusService = current.serviceStatus
				? services.find(
						(service) => service.id === current.serviceStatus?.service.id,
					)
				: undefined;
			return {
				...state,
				githubAccount:
					githubCatalogLoaded || githubCatalogLoading
						? (current.githubAccount ?? state.githubAccount)
						: state.githubAccount,
				repositories:
					githubCatalogLoaded || githubCatalogLoading
						? current.repositories
						: state.repositories,
				environment: state.environment ?? pendingEnvironment,
				environments: pendingEnvironments,
				services,
				service: selectedService,
				serviceStatus:
					current.serviceStatus && statusService
						? { ...current.serviceStatus, service: statusService }
						: state.serviceStatus,
			};
		});
	}, [state, githubCatalogLoaded, githubCatalogLoading]);

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
			const services = hydrateServiceSnapshots(
				JSON.parse((event as MessageEvent<string>).data),
			);
			mergeEnvironmentServices(services);
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
			const nextStatus = hydrateServiceStatusSnapshot(
				JSON.parse((event as MessageEvent<string>).data),
			);
			mergeStatusService(nextStatus);
		});
		return () => {
			active = false;
			source.close();
		};
	}, [selectedId, mergeStatusService]);

	const handleRefresh = () => {
		startTransition(() => {
			void router.invalidate();
		});
	};

	const handleNavigateEnvironment = (nextEnvironmentId: string | null) => {
		startTransition(() => {
			void router.navigate(
				nextEnvironmentId
					? {
							to: "/environments/$environmentId",
							params: { environmentId: nextEnvironmentId },
						}
					: { to: "/" },
			);
		});
	};

	const openNewService = () => {
		setShowNewService(true);
		void ensureGitHubCatalog();
	};

	const preloadNewService = () => {
		void loadNewServiceModal();
		void ensureGitHubCatalog();
	};

	const handleTabChange = (tab: DashboardTab) => {
		setActiveTab(tab);
		if (tab === "settings") {
			void ensureGitHubCatalog();
		}
	};

	const mergeService = (service: DashboardServiceRecord) => {
		setLocalState((current) => mergeHomeService(current, service));
	};

	const handleCreated = (result: CreateServiceFastResult) => {
		const existingService = services.some(
			(service) => service.id === result.service.id,
		);
		const spawnPosition =
			result.service.layoutPosition ??
			nextNodePositionNear(
				services.map(
					(service, index) =>
						canvas.servicePositions[service.id] ??
						service.layoutPosition ??
						nodePosition(index),
				),
			);

		if (!existingService && !result.service.layoutPosition) {
			canvas.setNodePositions((current) => ({
				...current,
				[result.service.id]: spawnPosition,
			}));
			canvas.nodePositionsRef.current = {
				...canvas.nodePositionsRef.current,
				[result.service.id]: spawnPosition,
			};
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

		previousEnvironmentIdRef.current = result.environment.id ?? null;
		rememberPendingCreatedService(result.service, result.environment);

		setLocalState((current) => {
			const nextServices = current.services.some(
				(service) => service.id === result.service.id,
			)
				? current.services.map((service) =>
						service.id === result.service.id ? result.service : service,
					)
				: [...current.services, result.service];
			const nextEnvironments = current.environments.some(
				(entry) => entry.id === result.environment.id,
			)
				? current.environments
				: [...current.environments, result.environment];
			return {
				...current,
				project:
					current.project?.id === result.project.id
						? current.project
						: result.project,
				environment: result.environment,
				environments: nextEnvironments,
				services: nextServices,
				service: result.service,
				serviceStatus: result.serviceStatus ?? current.serviceStatus,
				onboarding: result.onboarding,
				controlPlaneReachable: true,
				controlPlaneError: undefined,
			};
		});
		setSelectedId(result.service.id);
		canvas.hasUserPanned.current = false;
		setActiveTab("deployments");
		setShowNewService(false);
		if (environmentId && environmentId === result.environment.id) {
			startTransition(() => void router.invalidate());
		}
	};

	const handleServiceDeleted = (serviceId: string) => {
		clearPendingCreatedService(serviceId);
		setSelectedId((current) => (current === serviceId ? null : current));
		setLocalState((current) => ({
			...current,
			services: current.services.filter((service) => service.id !== serviceId),
			service: current.service?.id === serviceId ? undefined : current.service,
			serviceStatus:
				current.serviceStatus?.service.id === serviceId
					? undefined
					: current.serviceStatus,
		}));
		startTransition(() => void router.invalidate());
	};

	const deployServiceBatch = async (
		servicesToDeploy: Array<DashboardServiceRecord>,
		currentEnvironmentId: string,
	) => {
		const applying = servicesToDeploy.map(snapshotApplyingChanges);
		setDeployError(undefined);
		setShowChangeDetails(false);
		setDeployingChanges(true);
		setApplyingServices((current) => mergeApplyingServices(current, applying));
		try {
			const statuses = await doReleaseEnvironment({
				data: { environmentId: currentEnvironmentId },
			});
			for (const status of statuses) {
				mergeStatusService(status);
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
		try {
			const service = await doDiscardServiceChanges({
				data: { serviceId, discardAll: true },
			});
			mergeService(service);
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
		} finally {
			setDiscardingChangeId(undefined);
		}
	};

	const handleDiscardChange = async (serviceId: string, changeId: string) => {
		if (discardingChangeId) return;
		setDiscardingChangeId(`${serviceId}:${changeId}`);
		try {
			const service = await doDiscardServiceChanges({
				data: { serviceId, changeIds: [changeId] },
			});
			mergeService(service);
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
		} finally {
			setDiscardingChangeId(undefined);
		}
	};

	return (
		<div className="flex h-dvh flex-col overflow-hidden">
			<Topbar
				state={localState}
				onNewService={openNewService}
				onPreloadNewService={preloadNewService}
				onRefresh={handleRefresh}
				onNewEnvironment={() => setShowEnvironmentDialog(true)}
				onEnvironmentsChanged={handleRefresh}
				onNavigateEnvironment={handleNavigateEnvironment}
			/>
			{showEnvironmentDialog && (
				<EnvironmentDialog
					state={localState}
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
					setSelectedId(id);
					setActiveTab("deployments");
					setLocalState((current) => ({
						...current,
						serviceStatus: undefined,
					}));
				}}
				onResetView={canvas.handleResetView}
				onEscape={() => {
					clearPendingCreatedService(selectedId ?? undefined);
					setSelectedId(null);
					setLocalState((current) => ({
						...current,
						serviceStatus: undefined,
					}));
				}}
				panCursor={Boolean(canvas.panStart.current)}
				promptLeft={canvas.promptLeft}
				services={services}
				selectedId={selectedId}
				localState={localState}
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
					selected && "translate-x-0",
				)}
			>
				<div className="panel-grain" />
				{selected && (
					<Suspense fallback={<ServicePanelFallback service={selected} />}>
						<ServicePanel
							service={selected}
							status={liveStatus}
							project={localState.project}
							state={localState}
							activeTab={activeTab}
							onTabChange={handleTabChange}
							onClose={() => {
								clearPendingCreatedService(selectedId ?? undefined);
								setSelectedId(null);
								setLocalState((current) => ({
									...current,
									serviceStatus: undefined,
								}));
							}}
							onRefresh={handleRefresh}
							onServiceUpdated={mergeService}
							onServiceDeleted={handleServiceDeleted}
							onSpecSaveStateChange={setSelectedServiceWriteState}
						/>
					</Suspense>
				)}
			</div>

			{showNewService && (
				<Suspense fallback={null}>
					<NewServiceModal
						state={localState}
						catalogLoading={githubCatalogLoading}
						onClose={() => setShowNewService(false)}
						onCreated={handleCreated}
					/>
				</Suspense>
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
