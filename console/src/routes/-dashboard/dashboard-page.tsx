import { useRouter } from "@tanstack/react-router";
import { Loader2, RotateCcw, Trash2, UploadCloud, X } from "lucide-react";
import {
	lazy,
	type MouseEvent as ReactMouseEvent,
	Suspense,
	useCallback,
	useEffect,
	useLayoutEffect,
	useMemo,
	useRef,
	useState,
	useTransition,
} from "react";

import type {
	CreateServiceFastResult,
	DashboardEnvironment,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { EmptyCanvas } from "./empty-canvas";
import { EnvironmentDialog } from "./environment-dialog";
import { NODE_H, NODE_W, nextNodePositionNear, nodePosition } from "./layout";
import {
	doDeployEnvironment,
	doDiscardServiceChanges,
	doSaveServicePosition,
	fetchGitHubCatalog,
} from "./server-fns";
import { ServiceNode } from "./service-node";
import { formatError } from "./service-utils";
import { Topbar } from "./topbar";
import type { DashboardTab } from "./types";
import { ModalOverlay } from "./ui";

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

const CANVAS_SNAP = 32;
const CANVAS_MAJOR_GRID = 128;
const MIN_CANVAS_ZOOM = 0.3;
const MAX_CANVAS_ZOOM = 2;
const CANVAS_WHEEL_ZOOM_SENSITIVITY = 0.006;
const PENDING_CREATED_SERVICE_TTL_MS = 60_000;

type PendingCreatedService = {
	service: DashboardServiceRecord;
	environment: DashboardEnvironment;
	expiresAt: number;
};

// Survives the `/` → `/environments/:id` remount that happens when the first
// service materialises an environment. A component ref is wiped by that
// remount, which would close the service panel we just opened.
let pendingCreatedService: PendingCreatedService | null = null;

function rememberPendingCreatedService(
	service: DashboardServiceRecord,
	environment: DashboardEnvironment,
) {
	pendingCreatedService = {
		service,
		environment,
		expiresAt: Date.now() + PENDING_CREATED_SERVICE_TTL_MS,
	};
}

function clearPendingCreatedService(serviceId?: string) {
	if (!pendingCreatedService) return;
	if (serviceId && pendingCreatedService.service.id !== serviceId) return;
	pendingCreatedService = null;
}

function readPendingCreatedService(): PendingCreatedService | null {
	const pending = pendingCreatedService;
	if (!pending) return null;
	if (Date.now() > pending.expiresAt) {
		pendingCreatedService = null;
		return null;
	}
	return pending;
}

function withPendingCreatedService(
	list: Array<DashboardServiceRecord>,
): Array<DashboardServiceRecord> {
	const pending = readPendingCreatedService();
	if (!pending) return list;
	// Keep pending until the user closes the panel. A live snapshot that
	// already includes the service used to consume it, then `/` redirected
	// and remounted DashboardPage with nothing selected.
	if (list.some((service) => service.id === pending.service.id)) {
		return list;
	}
	return [...list, pending.service];
}

function hydrateStateWithPendingCreated(
	state: DashboardHomeState,
): DashboardHomeState {
	const pending = readPendingCreatedService();
	if (!pending) return state;
	const pendingEnvironment = !state.environment
		? pending.environment
		: undefined;
	const pendingEnvironments = pendingEnvironment
		? state.environments.some((entry) => entry.id === pendingEnvironment.id)
			? state.environments
			: [...state.environments, pendingEnvironment]
		: state.environments;
	return {
		...state,
		environment: state.environment ?? pendingEnvironment,
		environments: pendingEnvironments,
		services: withPendingCreatedService(state.services),
	};
}

export function resetDashboardPageTestState() {
	pendingCreatedService = null;
}

type Point = { x: number; y: number };
type ApplyingServiceChanges = {
	serviceId: string;
	serviceName: string;
	changeIds: Array<string>;
	count: number;
	specRevision?: number;
};

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
	const [liveStatus, setLiveStatus] = useState<DashboardServiceStatus | null>(
		null,
	);
	const [, setStatusLoading] = useState(false);
	const [showNewService, setShowNewService] = useState(false);
	const [showEnvironmentDialog, setShowEnvironmentDialog] = useState(false);
	const [githubCatalogLoading, setGitHubCatalogLoading] = useState(false);
	const [githubCatalogLoaded, setGitHubCatalogLoaded] = useState(
		state.repositories.length > 0,
	);
	const [deployingChanges, setDeployingChanges] = useState(false);
	const [deployQueued, setDeployQueued] = useState(false);
	const [applyingServices, setApplyingServices] = useState<
		Array<ApplyingServiceChanges>
	>([]);
	const [deployError, setDeployError] = useState<string>();
	const [showChangeDetails, setShowChangeDetails] = useState(false);
	const [discardingChangeId, setDiscardingChangeId] = useState<string>();
	const [promptLeft, setPromptLeft] = useState<number>();
	const [panOffset, setPanOffset] = useState<Point>({ x: 0, y: 0 });
	const [canvasSize, setCanvasSize] = useState({ width: 0, height: 0 });
	const [canvasReady, setCanvasReady] = useState(false);
	const targetPanOffset = useRef<Point>({ x: 0, y: 0 });
	const panAnimationFrame = useRef<number | null>(null);
	const [zoom, setZoom] = useState(1);
	const zoomRef = useRef(zoom);
	const [nodePositions, setNodePositions] = useState<Record<string, Point>>(
		() =>
			serviceLayoutPositions(hydrateStateWithPendingCreated(state).services),
	);
	const [, startTransition] = useTransition();
	const environmentId = localState.environment?.id ?? null;
	const panOffsetRef = useRef(panOffset);
	const nodePositionsRef = useRef(nodePositions);
	const dirtyServicesRef = useRef<Array<DashboardServiceRecord>>([]);
	const environmentIdRef = useRef<string | null>(environmentId);
	const previousEnvironmentIdRef = useRef<string | null>(environmentId);
	const deployQueuedRef = useRef(false);
	const githubCatalogPromiseRef = useRef<Promise<void> | null>(null);
	// Tracks spec revisions that were just deployed so that stale server data
	// returned by router.invalidate() (before a build rolls out) doesn't snap
	// the "Edited" badge / highlights back. Cleared once the server confirms
	// the service is clean or a newer spec has been saved.
	const deployedRevisionsRef = useRef<Map<string, number>>(new Map());
	const panStart = useRef<{
		mx: number;
		my: number;
		ox: number;
		oy: number;
	} | null>(null);
	const nodeDragStart = useRef<{
		serviceId: string;
		mx: number;
		my: number;
		ox: number;
		oy: number;
	} | null>(null);
	const isDragging = useRef(false);
	const suppressNextClick = useRef(false);
	const hasUserPanned = useRef(false);
	const centeredEnvironmentId = useRef<string | null | undefined>(undefined);
	const canvasRef = useRef<HTMLDivElement>(null);
	const selected = services.find((service) => service.id === selectedId);
	const dirtyServices = services.filter(hasUnappliedChanges);
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
	const deployableServices = dirtyServices.filter(
		(service) => queuedChangeCount(service, applyingChangeKeys) > 0,
	);
	const showPrompt =
		deployableUnappliedChanges > 0 ||
		applyingChangeCount > 0 ||
		deployQueued ||
		Boolean(deployError);
	const showCanvasSkeleton = services.length > 0 && !canvasReady;

	const startPanAnimation = useCallback(() => {
		if (panAnimationFrame.current !== null) return;
		const animate = () => {
			let settled = false;
			setPanOffset((prev) => {
				const tx = targetPanOffset.current.x;
				const ty = targetPanOffset.current.y;
				const dx = tx - prev.x;
				const dy = ty - prev.y;
				if (Math.abs(dx) < 0.5 && Math.abs(dy) < 0.5) {
					settled = true;
					if (prev.x === tx && prev.y === ty) return prev;
					return { x: tx, y: ty };
				}
				return {
					x: prev.x + dx * 0.12,
					y: prev.y + dy * 0.12,
				};
			});
			if (settled) {
				panAnimationFrame.current = null;
				return;
			}
			panAnimationFrame.current = requestAnimationFrame(animate);
		};
		panAnimationFrame.current = requestAnimationFrame(animate);
	}, []);

	const setTargetPanOffset = useCallback(
		(next: Point, options: { immediate?: boolean } = {}) => {
			targetPanOffset.current = next;
			if (options.immediate) {
				if (panAnimationFrame.current !== null) {
					cancelAnimationFrame(panAnimationFrame.current);
					panAnimationFrame.current = null;
				}
				setPanOffset(next);
				return;
			}
			startPanAnimation();
		},
		[startPanAnimation],
	);

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
		panOffsetRef.current = panOffset;
	}, [panOffset]);

	useEffect(() => {
		zoomRef.current = zoom;
	}, [zoom]);

	useEffect(() => {
		nodePositionsRef.current = nodePositions;
	}, [nodePositions]);

	useEffect(() => {
		dirtyServicesRef.current = dirtyServices;
	}, [dirtyServices]);

	useEffect(() => {
		environmentIdRef.current = environmentId;
	}, [environmentId]);

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
			setLiveStatus(null);
		}
		setNodePositions(serviceLayoutPositions(localState.services));
		nodePositionsRef.current = serviceLayoutPositions(localState.services);
	}, [environmentId, localState.services]);

	useLayoutEffect(() => {
		const canvas = canvasRef.current;
		if (!canvas) return;

		const updateCanvasSize = () => {
			const width = canvas.clientWidth;
			const height = canvas.clientHeight;
			setCanvasSize((current) =>
				current.width === width && current.height === height
					? current
					: { width, height },
			);
		};

		updateCanvasSize();
		if (typeof ResizeObserver === "undefined") {
			window.addEventListener("resize", updateCanvasSize);
			return () => window.removeEventListener("resize", updateCanvasSize);
		}

		const observer = new ResizeObserver(updateCanvasSize);
		observer.observe(canvas);
		return () => observer.disconnect();
	}, []);

	const persistNodePositions = useCallback(
		(serviceId: string, position: Point) => {
			if (!environmentId) return;
			void doSaveServicePosition({
				data: { environmentId, serviceId, position },
			});
		},
		[environmentId],
	);

	const servicePositions = useMemo(() => {
		const positions: Record<string, Point> = {};
		services.forEach((service, index) => {
			positions[service.id] =
				nodePositions[service.id] ??
				service.layoutPosition ??
				nodePosition(index);
		});
		return positions;
	}, [services, nodePositions]);

	useLayoutEffect(() => {
		if (
			!canvasRef.current ||
			canvasSize.width === 0 ||
			canvasSize.height === 0 ||
			services.length === 0 ||
			(canvasReady &&
				(selectedId ||
					hasUserPanned.current ||
					centeredEnvironmentId.current === environmentId))
		) {
			return;
		}
		const next = centeredServicesPanOffset({
			canvas: canvasRef.current,
			services,
			servicePositions,
			zoom,
		});
		if (!next) return;
		centeredEnvironmentId.current = environmentId;
		setTargetPanOffset(next, { immediate: true });
		setCanvasReady(true);
	}, [
		canvasReady,
		canvasSize,
		environmentId,
		selectedId,
		servicePositions,
		setTargetPanOffset,
		services,
		zoom,
	]);

	useEffect(() => {
		if (!selectedId || !canvasRef.current || hasUserPanned.current) return;
		const pos = servicePositions[selectedId];
		if (!pos) return;
		const canvas = canvasRef.current;
		const sidePanelOpen = Boolean(selected);
		const visibleWidth = sidePanelOpen
			? canvas.clientWidth * 0.4
			: canvas.clientWidth;
		setTargetPanOffset({
			x: (visibleWidth - NODE_W) / 2 - pos.x,
			y: (canvas.clientHeight - NODE_H) / 2 - pos.y,
		});
	}, [selectedId, servicePositions, selected, setTargetPanOffset]);

	useEffect(() => {
		const updatePromptLeft = () => {
			const canvas = canvasRef.current;
			if (!canvas) return;
			const mobilePanelOpen =
				selected &&
				typeof window.matchMedia === "function" &&
				window.matchMedia("(max-width: 900px)").matches;
			if (mobilePanelOpen) {
				setPromptLeft(undefined);
				return;
			}
			const sidePanelWidth = selected ? window.innerWidth * 0.6 : 0;
			const visibleWidth = Math.max(0, canvas.clientWidth - sidePanelWidth);
			setPromptLeft(visibleWidth / 2);
		};
		updatePromptLeft();
		window.addEventListener("resize", updatePromptLeft);
		return () => window.removeEventListener("resize", updatePromptLeft);
	}, [selected]);

	useEffect(() => {
		return () => {
			if (panAnimationFrame.current !== null) {
				cancelAnimationFrame(panAnimationFrame.current);
				panAnimationFrame.current = null;
			}
		};
	}, []);

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
				setLiveStatus(null);
			}
		};
		document.addEventListener("keydown", onKeyDown);
		return () => document.removeEventListener("keydown", onKeyDown);
	}, [selectedId, showChangeDetails, showNewService]);

	useEffect(() => {
		const canvas = canvasRef.current;
		if (!canvas) return;
		const onWheel = (event: WheelEvent) => {
			const target = event.target as HTMLElement | null;
			if (
				target?.closest(
					".dirty-workspace-banner, input, select, textarea, [role='button']:not(.service-node), button:not(.service-node), a",
				)
			) {
				return;
			}
			event.preventDefault();
			hasUserPanned.current = true;

			const delta = normalizeWheelDelta(event);
			const currentZoom = zoomRef.current;
			if (event.ctrlKey || event.metaKey) {
				const nextZoom = clampCanvasZoom(
					currentZoom * Math.exp(-delta.y * CANVAS_WHEEL_ZOOM_SENSITIVITY),
				);
				if (nextZoom === currentZoom) return;
				const rect = canvas.getBoundingClientRect();
				const pointer = {
					x: event.clientX - rect.left,
					y: event.clientY - rect.top,
				};
				const currentPan = panOffsetRef.current;
				const worldPoint = {
					x: (pointer.x - currentPan.x) / currentZoom,
					y: (pointer.y - currentPan.y) / currentZoom,
				};
				const nextPan = {
					x: pointer.x - worldPoint.x * nextZoom,
					y: pointer.y - worldPoint.y * nextZoom,
				};
				zoomRef.current = nextZoom;
				targetPanOffset.current = nextPan;
				setZoom(nextZoom);
				setPanOffset(nextPan);
				return;
			}

			const nextPan = {
				x: panOffsetRef.current.x - delta.x,
				y: panOffsetRef.current.y - delta.y,
			};
			targetPanOffset.current = nextPan;
			setPanOffset(nextPan);
		};
		canvas.addEventListener("wheel", onWheel, { passive: false });
		return () => canvas.removeEventListener("wheel", onWheel);
	}, []);

	const suppressJustDeployedChanges = useCallback(
		(service: DashboardServiceRecord) => {
			// Suppress unapplied changes for services that were just deployed. The
			// backend may still report them while the build/rollout catches up, but
			// the user has already applied those changes.
			const deployedMap = deployedRevisionsRef.current;
			const deployedRevision = deployedMap.get(service.id);
			if (deployedRevision === undefined) return service;
			if ((service.unappliedChangeCount ?? 0) === 0) {
				deployedMap.delete(service.id);
				return service;
			}
			if ((service.specRevision ?? 0) > deployedRevision) {
				deployedMap.delete(service.id);
				return service;
			}
			return {
				...service,
				unappliedChanges: [],
				unappliedChangeCount: 0,
				pendingChanges: false,
			};
		},
		[],
	);

	const mergeStatusService = useCallback(
		(status: DashboardServiceStatus) => {
			// Suppress unapplied changes for services that were just deployed — the
			// SSE stream continuously pushes status updates whose service.unappliedChanges
			// still reflects the backend's "pending build" state, which would otherwise
			// overwrite the optimistic clearing we did on Deploy.
			const mergedService = suppressJustDeployedChanges(status.service);
			setLocalState((current) => ({
				...current,
				services: current.services.map((entry) =>
					entry.id === mergedService.id ? mergedService : entry,
				),
				service:
					current.service?.id === mergedService.id
						? mergedService
						: current.service,
				serviceStatus:
					current.serviceStatus?.service.id === mergedService.id
						? { ...current.serviceStatus, ...status }
						: current.serviceStatus,
			}));
		},
		[suppressJustDeployedChanges],
	);

	const mergeEnvironmentServices = useCallback(
		(nextServices: Array<DashboardServiceRecord>) => {
			setLocalState((current) => {
				const mergedServices = withPendingCreatedService(
					nextServices.map(suppressJustDeployedChanges),
				);
				const selectedService =
					current.service &&
					mergedServices.find((service) => service.id === current.service?.id);
				const statusService =
					current.serviceStatus &&
					mergedServices.find(
						(service) => service.id === current.serviceStatus?.service.id,
					);
				return {
					...current,
					services: mergedServices,
					service: selectedService,
					serviceStatus:
						current.serviceStatus && statusService
							? { ...current.serviceStatus, service: statusService }
							: undefined,
				};
			});
			setLiveStatus((current) => {
				if (!current) return current;
				const service = nextServices
					.map(suppressJustDeployedChanges)
					.find((entry) => entry.id === current.service.id);
				return service ? { ...current, service } : current;
			});
		},
		[suppressJustDeployedChanges],
	);

	const mergeAppliedStatusService = useCallback(
		(status: DashboardServiceStatus, applied: ApplyingServiceChanges) => {
			const statusRevision = status.service.specRevision ?? 0;
			setLocalState((current) => {
				const existing = current.services.find(
					(service) => service.id === status.service.id,
				);
				const existingRevision = existing?.specRevision ?? 0;
				if (
					existing &&
					applied.specRevision !== undefined &&
					existingRevision > applied.specRevision &&
					statusRevision <= applied.specRevision
				) {
					return {
						...current,
						service:
							current.service?.id === status.service.id
								? existing
								: current.service,
						serviceStatus:
							current.serviceStatus?.service.id === status.service.id
								? { ...status, service: existing }
								: current.serviceStatus,
					};
				}
				return {
					...current,
					services: current.services.map((entry) =>
						entry.id === status.service.id ? status.service : entry,
					),
					service:
						current.service?.id === status.service.id
							? status.service
							: current.service,
					serviceStatus:
						current.serviceStatus?.service.id === status.service.id
							? { ...current.serviceStatus, ...status }
							: current.serviceStatus,
				};
			});
			setLiveStatus((current) => {
				if (current?.service.id !== status.service.id) return current;
				const currentRevision = current.service.specRevision ?? 0;
				if (
					applied.specRevision !== undefined &&
					currentRevision > applied.specRevision &&
					statusRevision <= applied.specRevision
				) {
					return { ...status, service: current.service };
				}
				return status;
			});
		},
		[],
	);

	useEffect(() => {
		const deployedMap = deployedRevisionsRef.current;
		// A just-created service can also be the first one to materialise its
		// environment; keep that environment until the server snapshot has it,
		// otherwise the environment flips back to none and the selection resets.
		const pending = readPendingCreatedService();
		const pendingEnvironment =
			!state.environment && pending ? pending.environment : undefined;
		const pendingEnvironments = pendingEnvironment
			? state.environments.some((entry) => entry.id === pendingEnvironment.id)
				? state.environments
				: [...state.environments, pendingEnvironment]
			: state.environments;
		if (deployedMap.size === 0) {
			setLocalState((current) => ({
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
				services: withPendingCreatedService(state.services),
			}));
		} else {
			// For services that were just deployed, the backend may still report
			// unapplied changes until the build / rollout completes. Suppress them
			// so the "Edited" badge and panel highlights stay gone after Deploy.
			setLocalState((current) => ({
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
				services: withPendingCreatedService(state.services).map((service) => {
					const deployedRevision = deployedMap.get(service.id);
					if (deployedRevision === undefined) return service;
					// Server confirms clean — stop suppressing.
					if ((service.unappliedChangeCount ?? 0) === 0) {
						deployedMap.delete(service.id);
						return service;
					}
					// A newer spec was saved after the deploy — show its changes.
					if ((service.specRevision ?? 0) > deployedRevision) {
						deployedMap.delete(service.id);
						return service;
					}
					// Same spec, still building — keep changes hidden.
					return {
						...service,
						unappliedChanges: [],
						unappliedChangeCount: 0,
						pendingChanges: false,
					};
				}),
			}));
		}
		setStatusLoading(false);
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
			setLiveStatus(null);
			setStatusLoading(false);
			return;
		}
		setStatusLoading(true);
		let active = true;
		const source = new EventSource(
			`/events/service-status?serviceId=${encodeURIComponent(selectedId)}`,
		);
		source.addEventListener("status", (event) => {
			if (!active) return;
			const nextStatus = hydrateServiceStatusSnapshot(
				JSON.parse((event as MessageEvent<string>).data),
			);
			setLiveStatus(nextStatus);
			mergeStatusService(nextStatus);
			setStatusLoading(false);
		});
		source.addEventListener("status-error", () => {
			if (!active) return;
			setStatusLoading(false);
		});
		source.onerror = () => {
			if (!active) return;
			setStatusLoading(false);
		};
		return () => {
			active = false;
			source.close();
		};
	}, [selectedId, mergeStatusService]);

	const onCanvasMouseDown = useCallback(
		(event: ReactMouseEvent<HTMLElement>) => {
			const el = event.target as HTMLElement;
			if (
				el.closest(
					".service-node, .dirty-workspace-banner, button, a, input, select, textarea, [role='button']",
				)
			) {
				return;
			}
			panStart.current = {
				mx: event.clientX,
				my: event.clientY,
				ox: panOffset.x,
				oy: panOffset.y,
			};
			isDragging.current = false;
		},
		[panOffset],
	);

	const onServiceMouseDown = useCallback(
		(serviceId: string, event: ReactMouseEvent<HTMLElement>) => {
			if (event.button !== 0) return;
			const el = event.target as HTMLElement;
			if (el.closest("a, input, select, textarea, [role='button']")) {
				return;
			}
			const pos = servicePositions[serviceId];
			if (!pos) return;
			targetPanOffset.current = panOffset;
			nodeDragStart.current = {
				serviceId,
				mx: event.clientX,
				my: event.clientY,
				ox: pos.x,
				oy: pos.y,
			};
			isDragging.current = false;
		},
		[panOffset, servicePositions],
	);

	useEffect(() => {
		const onMove = (event: globalThis.MouseEvent) => {
			if (nodeDragStart.current) {
				const drag = nodeDragStart.current;
				const dx = (event.clientX - drag.mx) / zoom;
				const dy = (event.clientY - drag.my) / zoom;
				if (Math.abs(dx) + Math.abs(dy) > 4) {
					isDragging.current = true;
					hasUserPanned.current = true;
					targetPanOffset.current = panOffsetRef.current;
				}
				const next = {
					x: snapToCanvas(drag.ox + dx),
					y: snapToCanvas(drag.oy + dy),
				};
				setNodePositions((current) => ({
					...current,
					[drag.serviceId]: next,
				}));
				nodePositionsRef.current = {
					...nodePositionsRef.current,
					[drag.serviceId]: next,
				};
				return;
			}
			if (!panStart.current) return;
			const dx = event.clientX - panStart.current.mx;
			const dy = event.clientY - panStart.current.my;
			if (Math.abs(dx) + Math.abs(dy) > 4) {
				isDragging.current = true;
				hasUserPanned.current = true;
			}
			const next = {
				x: panStart.current.ox + dx,
				y: panStart.current.oy + dy,
			};
			targetPanOffset.current = next;
			setPanOffset(next);
		};
		const onUp = () => {
			const draggedNode = nodeDragStart.current;
			if (isDragging.current) {
				suppressNextClick.current = true;
				window.setTimeout(() => {
					isDragging.current = false;
					suppressNextClick.current = false;
				}, 0);
			}
			if (draggedNode) {
				const position = nodePositionsRef.current[draggedNode.serviceId];
				if (position) {
					persistNodePositions(draggedNode.serviceId, position);
				}
			}
			panStart.current = null;
			nodeDragStart.current = null;
		};
		window.addEventListener("mousemove", onMove);
		window.addEventListener("mouseup", onUp);
		return () => {
			window.removeEventListener("mousemove", onMove);
			window.removeEventListener("mouseup", onUp);
		};
	}, [persistNodePositions, zoom]);

	const onCanvasClick = (event: ReactMouseEvent<HTMLElement>) => {
		if (isDragging.current || suppressNextClick.current) return;
		const el = event.target as HTMLElement;
		if (
			el.closest(
				".dirty-workspace-banner, button, a, input, select, textarea, [role='button']",
			)
		) {
			return;
		}
		if (!el.closest(".service-node")) {
			clearPendingCreatedService(selectedId ?? undefined);
			setSelectedId(null);
			setLiveStatus(null);
		}
	};

	const selectService = (id: string) => {
		if (isDragging.current || suppressNextClick.current) return;
		hasUserPanned.current = false;
		setSelectedId(id);
		setActiveTab("deployments");
		setLiveStatus(null);
	};

	const handleRefresh = () => {
		startTransition(() => {
			void router.invalidate();
		});
	};

	// Environment switching is a route change, not a page load — a full reload
	// would throw away the canvas, the SSE streams, and the selection.
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
		setLocalState((current) => ({
			...current,
			services: current.services.map((entry) =>
				entry.id === service.id ? service : entry,
			),
			service: current.service?.id === service.id ? service : current.service,
			serviceStatus:
				current.serviceStatus?.service.id === service.id
					? { ...current.serviceStatus, service }
					: current.serviceStatus,
		}));
		setLiveStatus((current) =>
			current?.service.id === service.id ? { ...current, service } : current,
		);
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
						servicePositions[service.id] ??
						service.layoutPosition ??
						nodePosition(index),
				),
			);

		if (!existingService && !result.service.layoutPosition) {
			setNodePositions((current) => ({
				...current,
				[result.service.id]: spawnPosition,
			}));
			nodePositionsRef.current = {
				...nodePositionsRef.current,
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

		// Creating the first service can materialise the project's environment,
		// which flips `environmentId` from null. Claim that transition here so the
		// environment-change effect doesn't immediately clear the selection and
		// close the panel we are about to open for the new service.
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
		hasUserPanned.current = false;
		setActiveTab("deployments");
		setLiveStatus(result.serviceStatus);
		setShowNewService(false);
		// Creating the first service also creates the environment. Invalidating
		// `/` redirects to `/environments/:id` and remounts this page, which
		// closed the panel. Stay on the current route until the next explicit
		// navigation; pending state keeps the panel open if a remount happens.
		if (environmentId && environmentId === result.environment.id) {
			startTransition(() => void router.invalidate());
		}
	};

	const handleServiceDeleted = (serviceId: string) => {
		clearPendingCreatedService(serviceId);
		deployedRevisionsRef.current.delete(serviceId);
		setSelectedId((current) => (current === serviceId ? null : current));
		setLiveStatus(null);
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

	const handleResetView = () => {
		const next = { x: 0, y: 0 };
		setZoom(1);
		setTargetPanOffset(next);
		hasUserPanned.current = false;
	};

	const deployServiceBatch = async (
		servicesToDeploy: Array<DashboardServiceRecord>,
		currentEnvironmentId: string,
	) => {
		const applying = servicesToDeploy.map(snapshotApplyingChanges);
		const deployingIds = new Set(applying.map((a) => a.serviceId));
		// Remember the spec revision at deploy time so the state effect can
		// suppress stale "still unapplied" data that comes back from the server
		// before the build / rollout actually completes.
		for (const a of applying) {
			deployedRevisionsRef.current.set(a.serviceId, a.specRevision ?? 0);
		}
		deployQueuedRef.current = false;
		setDeployQueued(false);
		setDeployError(undefined);
		setShowChangeDetails(false);
		setDeployingChanges(true);
		setApplyingServices((current) => mergeApplyingServices(current, applying));
		// Optimistically clear unapplied changes so nodes/panel/banner all snap
		// clean immediately — if the server returns new changes they'll re-appear.
		setLocalState((current) => ({
			...current,
			services: current.services.map((service) =>
				deployingIds.has(service.id)
					? {
							...service,
							unappliedChanges: [],
							unappliedChangeCount: 0,
							pendingChanges: false,
						}
					: service,
			),
		}));
		try {
			const statuses = await doDeployEnvironment({
				data: { environmentId: currentEnvironmentId },
			});
			for (const status of statuses) {
				const service = applying.find(
					(entry) => entry.serviceId === status.service.id,
				);
				if (!service) continue;
				// Strip unapplied changes from the response: for source-based services
				// the backend still reports them as unapplied until the build rolls out,
				// but the user just deployed — they are in flight, not pending.
				mergeAppliedStatusService(
					{
						...status,
						service: {
							...status.service,
							unappliedChanges: [],
							unappliedChangeCount: 0,
							pendingChanges: false,
						},
					},
					service,
				);
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
			window.setTimeout(() => {
				if (!deployQueuedRef.current) return;
				const nextEnvironmentId = environmentIdRef.current;
				const nextDirtyServices = dirtyServicesRef.current;
				if (!nextEnvironmentId || nextDirtyServices.length === 0) {
					deployQueuedRef.current = false;
					setDeployQueued(false);
					return;
				}
				void deployServiceBatch(nextDirtyServices, nextEnvironmentId);
			}, 0);
		}
	};

	const handleDeployChanges = async () => {
		if (!environmentId) {
			return;
		}
		if (deployingChanges) {
			if (deployableServices.length === 0 || deployQueued) return;
			deployQueuedRef.current = true;
			setDeployQueued(true);
			return;
		}
		if (deployableServices.length === 0) return;
		await deployServiceBatch(deployableServices, environmentId);
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
		<div
			className="app-canvas"
			style={{ display: "flex", flexDirection: "column" }}
		>
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
			<div
				role="application"
				tabIndex={-1}
				style={{
					flex: 1,
					marginTop: "var(--dashboard-header-height)",
					position: "relative",
					overflow: "hidden",
					cursor: panStart.current ? "grabbing" : "grab",
				}}
				ref={canvasRef}
				className="canvas-grid"
				onMouseDown={onCanvasMouseDown}
				onClick={onCanvasClick}
				onKeyDown={(event) => {
					if (event.key === "Escape") {
						clearPendingCreatedService(selectedId ?? undefined);
						setSelectedId(null);
						setLiveStatus(null);
					}
				}}
			>
				{showCanvasSkeleton && <DashboardCanvasSkeleton />}
				<div
					className="canvas-world"
					style={{
						transform: `translate(${panOffset.x}px,${panOffset.y}px) scale(${zoom})`,
						visibility:
							canvasReady || services.length === 0 ? "visible" : "hidden",
					}}
				>
					<div className="canvas-world-grid" />
					{services.map((service, index) => (
						<ServiceNode
							key={service.id}
							service={service}
							pos={servicePositions[service.id] ?? nodePosition(index)}
							selected={service.id === selectedId}
							onMouseDown={(event) => onServiceMouseDown(service.id, event)}
							onSelect={() => selectService(service.id)}
						/>
					))}
				</div>

				{services.length === 0 && !showNewService && (
					<EmptyCanvas
						state={localState}
						onAdd={openNewService}
						onPreloadAdd={preloadNewService}
					/>
				)}

				{showPrompt && (
					<div
						className="dirty-workspace-banner"
						style={
							promptLeft === undefined
								? { display: "none" }
								: { left: promptLeft }
						}
					>
						<div>
							<span className="dirty-workspace-title">Undeployed changes</span>
							<span className="dirty-workspace-detail">
								{dirtyPromptDetail({
									applying: applyingChangeCount,
									deployQueued,
									deployable: deployableUnappliedChanges,
									total: totalUnappliedChanges,
								})}
							</span>
							{deployError && (
								<span className="dirty-workspace-error">{deployError}</span>
							)}
						</div>
						<button
							type="button"
							className="btn-secondary"
							onClick={() => setShowChangeDetails(true)}
						>
							Details
						</button>
						<button
							type="button"
							className="btn-primary"
							onClick={handleDeployChanges}
							disabled={
								(deployingChanges && deployQueued) ||
								deployableUnappliedChanges === 0
							}
						>
							{deployingChanges ? (
								<Loader2
									size={13}
									style={{ animation: "spin 1s linear infinite" }}
								/>
							) : (
								<UploadCloud size={13} />
							)}
							{deployingChanges
								? deployQueued
									? "Queued"
									: "Queue deploy"
								: "Deploy"}
						</button>
					</div>
				)}

				<div className="zoom-controls">
					<button
						type="button"
						className="zoom-btn"
						aria-label="Zoom out"
						title="Zoom out"
						onClick={() =>
							setZoom((current) => clampCanvasZoom(+(current - 0.1).toFixed(1)))
						}
					>
						−
					</button>
					<button
						type="button"
						className="zoom-btn"
						aria-label="Zoom in"
						title="Zoom in"
						onClick={() =>
							setZoom((current) => clampCanvasZoom(+(current + 0.1).toFixed(1)))
						}
					>
						+
					</button>
					<button
						type="button"
						className="zoom-btn"
						aria-label="Reset zoom"
						title="Reset zoom"
						onClick={handleResetView}
					>
						<RotateCcw size={14} />
					</button>
				</div>
			</div>

			<div className={`side-panel ${selected ? "open" : ""}`}>
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
								setLiveStatus(null);
							}}
							onRefresh={handleRefresh}
							onServiceUpdated={mergeService}
							onServiceDeleted={handleServiceDeleted}
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
					deployQueued={deployQueued}
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

export function DashboardCanvasSkeleton({
	showTopbar = false,
}: {
	showTopbar?: boolean;
}) {
	return (
		<div
			className={showTopbar ? "app-canvas canvas-skeleton-page" : undefined}
			aria-hidden="true"
		>
			{showTopbar && (
				<div className="topbar topbar-skeleton">
					<span className="skeleton-mark" />
					<span className="skeleton-line skeleton-title" />
					<span className="skeleton-pill" />
					<span className="skeleton-line skeleton-count" />
					<span className="skeleton-spacer" />
					<span className="skeleton-icon" />
					<span className="skeleton-button" />
					<span className="skeleton-icon" />
				</div>
			)}
			<div
				className="canvas-grid canvas-skeleton-stage"
				style={
					showTopbar
						? { marginTop: "var(--dashboard-header-height)" }
						: undefined
				}
			>
				<div className="canvas-world-grid" />
				<div className="canvas-loading-skeleton">
					<SkeletonNode />
				</div>
			</div>
		</div>
	);
}

function ServicePanelFallback({
	service,
}: {
	service: DashboardServiceRecord;
}) {
	return (
		<>
			<div className="panel-crumbs">
				<strong>{service.name}</strong>
				<span style={{ flex: 1 }} />
				<Loader2 size={13} style={{ animation: "spin 1s linear infinite" }} />
			</div>
			<div className="service-panel-content">
				<div className="service-panel-scroll">
					<div className="panel-loading-row">
						<Loader2
							size={13}
							style={{ animation: "spin 1s linear infinite" }}
						/>
					</div>
				</div>
			</div>
		</>
	);
}

function SkeletonNode() {
	return (
		<div
			className="service-node skeleton-node"
			style={{ width: NODE_W, height: NODE_H }}
		>
			<div className="skeleton-node-head">
				<span className="skeleton-status" />
				<span className="skeleton-line skeleton-node-title" />
			</div>
			<div className="skeleton-node-body">
				<div className="skeleton-meta-row">
					<span className="skeleton-icon-inline" />
					<span className="skeleton-line skeleton-row row-1" />
				</div>
				<div className="skeleton-meta-row">
					<span className="skeleton-icon-inline" />
					<span className="skeleton-line skeleton-row row-2" />
				</div>
				<div className="skeleton-node-footer">
					<span className="node-deploy-badge skeleton-deploy-badge">
						<span className="skeleton-badge-icon" />
						<span className="node-badge-rail">
							<span className="panel-badge-segment" />
							<span className="panel-badge-segment" />
							<span className="panel-badge-segment" />
						</span>
					</span>
					<span className="skeleton-line skeleton-sha" />
				</div>
			</div>
		</div>
	);
}

function UnappliedChangesDialog({
	services,
	totalChanges,
	applyingChanges,
	deployableChanges,
	deployQueued,
	affectedServices,
	deploying,
	deployError,
	discardingChangeId,
	onClose,
	onDeploy,
	onDiscardChange,
	onDiscardService,
}: {
	services: Array<DashboardServiceRecord>;
	totalChanges: number;
	applyingChanges: number;
	deployableChanges: number;
	deployQueued: boolean;
	affectedServices: number;
	deploying: boolean;
	deployError?: string;
	discardingChangeId?: string;
	onClose: () => void;
	onDeploy: () => void;
	onDiscardChange: (serviceId: string, changeId: string) => void;
	onDiscardService: (serviceId: string) => void;
}) {
	return (
		<ModalOverlay onClose={onClose}>
			<div className="modal-card unapplied-dialog">
				<div className="unapplied-dialog-header">
					<h2>{totalChanges} changes to apply</h2>
					<button
						type="button"
						className="icon-btn"
						aria-label="Close"
						onClick={onClose}
					>
						<X size={16} />
					</button>
				</div>
				<div className="unapplied-service-list">
					{services.map((service) => (
						<div className="unapplied-service-group" key={service.id}>
							<div className="unapplied-service-heading">
								<div>
									<strong>{service.name}</strong>
									<span>{sectionSummary(service.unappliedChanges ?? [])}</span>
								</div>
								<button
									type="button"
									className="btn-secondary"
									onClick={() => onDiscardService(service.id)}
									disabled={discardingChangeId === `service:${service.id}`}
								>
									<Trash2 size={13} />
									Discard service changes
								</button>
							</div>
							{(service.unappliedChanges ?? []).map((change) => (
								<div className="unapplied-change-row" key={change.id}>
									<span className={`change-action ${change.action}`}>
										{change.action}
									</span>
									<div className="change-field">
										<strong>{change.field}</strong>
										<span>{change.section}</span>
									</div>
									<div className="change-values">
										{change.currentValue && (
											<div>
												<span>Current</span>
												<code>{change.currentValue}</code>
											</div>
										)}
										{!change.currentValue && (
											<div className="change-value-spacer" aria-hidden="true" />
										)}
										<div>
											<span>New</span>
											<code>{change.newValue || "empty"}</code>
										</div>
									</div>
									<button
										type="button"
										className="icon-btn"
										aria-label={`Discard ${change.field}`}
										onClick={() => onDiscardChange(service.id, change.id)}
										disabled={
											discardingChangeId === `${service.id}:${change.id}`
										}
									>
										<Trash2 size={14} />
									</button>
								</div>
							))}
						</div>
					))}
				</div>
				<div className="unapplied-dialog-footer">
					<span>
						{dirtyPromptDetail({
							applying: applyingChanges,
							deployQueued,
							deployable: deployableChanges,
							total: totalChanges,
						})}{" "}
						across {affectedServices}{" "}
						{affectedServices === 1 ? "service" : "services"}
					</span>
					{deployError && (
						<span className="dirty-workspace-error">{deployError}</span>
					)}
					<button
						type="button"
						className="btn-primary"
						onClick={onDeploy}
						disabled={(deploying && deployQueued) || deployableChanges === 0}
					>
						{deploying ? (
							<Loader2
								size={13}
								style={{ animation: "spin 1s linear infinite" }}
							/>
						) : (
							<UploadCloud size={13} />
						)}
						{deploying
							? deployQueued
								? "Queued"
								: "Queue Deploy"
							: "Deploy Changes"}
					</button>
				</div>
			</div>
		</ModalOverlay>
	);
}

function snapToCanvas(value: number): number {
	return Math.round(value / CANVAS_SNAP) * CANVAS_SNAP;
}

function clampCanvasZoom(value: number): number {
	return Math.min(MAX_CANVAS_ZOOM, Math.max(MIN_CANVAS_ZOOM, value));
}

function normalizeWheelDelta(event: WheelEvent): Point {
	const multiplier =
		event.deltaMode === WheelEvent.DOM_DELTA_LINE
			? 16
			: event.deltaMode === WheelEvent.DOM_DELTA_PAGE
				? window.innerHeight
				: 1;
	return {
		x: event.deltaX * multiplier,
		y: event.deltaY * multiplier,
	};
}

function centeredServicesPanOffset({
	canvas,
	services,
	servicePositions,
	zoom,
}: {
	canvas: { clientWidth: number; clientHeight: number };
	services: Array<DashboardServiceRecord>;
	servicePositions: Record<string, Point>;
	zoom: number;
}): Point | null {
	let minX = Number.POSITIVE_INFINITY;
	let minY = Number.POSITIVE_INFINITY;
	let maxX = Number.NEGATIVE_INFINITY;
	let maxY = Number.NEGATIVE_INFINITY;
	for (const service of services) {
		const position = servicePositions[service.id];
		if (!position) continue;
		minX = Math.min(minX, position.x);
		minY = Math.min(minY, position.y);
		maxX = Math.max(maxX, position.x + NODE_W);
		maxY = Math.max(maxY, position.y + NODE_H);
	}
	if (
		!Number.isFinite(minX) ||
		!Number.isFinite(minY) ||
		!Number.isFinite(maxX) ||
		!Number.isFinite(maxY)
	) {
		return null;
	}
	const worldCenterX = (minX + maxX) / 2;
	const worldCenterY = (minY + maxY) / 2;
	return {
		x: snapPanToMajorGrid(canvas.clientWidth / 2 - worldCenterX * zoom, zoom),
		y: snapPanToMajorGrid(canvas.clientHeight / 2 - worldCenterY * zoom, zoom),
	};
}

function snapPanToMajorGrid(value: number, zoom: number): number {
	const size = CANVAS_MAJOR_GRID * zoom;
	return Math.round(value / size) * size;
}

function serviceLayoutPositions(
	services: Array<DashboardServiceRecord>,
): Record<string, Point> {
	const positions: Record<string, Point> = {};
	for (const service of services) {
		if (service.layoutPosition) {
			positions[service.id] = service.layoutPosition;
		}
	}
	return positions;
}

function unappliedChangeCount(service: DashboardServiceRecord): number {
	return service.unappliedChangeCount ?? (service.pendingChanges ? 1 : 0);
}

function hasUnappliedChanges(service: DashboardServiceRecord): boolean {
	return unappliedChangeCount(service) > 0;
}

function snapshotApplyingChanges(
	service: DashboardServiceRecord,
): ApplyingServiceChanges {
	const changes = service.unappliedChanges ?? [];
	const count = unappliedChangeCount(service);
	return {
		serviceId: service.id,
		serviceName: service.name,
		changeIds:
			changes.length > 0
				? changes.map((change) => change.id)
				: [`service:${service.id}:${service.specRevision ?? "current"}`],
		count,
		specRevision: service.specRevision,
	};
}

function mergeApplyingServices(
	current: Array<ApplyingServiceChanges>,
	next: Array<ApplyingServiceChanges>,
): Array<ApplyingServiceChanges> {
	const services = new Map<string, ApplyingServiceChanges>();
	for (const service of current) {
		services.set(service.serviceId, service);
	}
	for (const service of next) {
		services.set(service.serviceId, service);
	}
	return Array.from(services.values());
}

function queuedChangeCount(
	service: DashboardServiceRecord,
	applyingChangeKeys: Set<string>,
): number {
	const changes = service.unappliedChanges ?? [];
	if (changes.length === 0) {
		return applyingChangeKeys.has(
			applyingChangeKey(
				service.id,
				`service:${service.id}:${service.specRevision ?? "current"}`,
			),
		)
			? 0
			: unappliedChangeCount(service);
	}
	return changes.filter(
		(change) =>
			!applyingChangeKeys.has(applyingChangeKey(service.id, change.id)),
	).length;
}

function applyingChangeKey(serviceId: string, changeId: string): string {
	return `${serviceId}:${changeId}`;
}

function dirtyPromptDetail({
	applying,
	deployQueued,
	deployable,
	total,
}: {
	applying: number;
	deployQueued: boolean;
	deployable: number;
	total: number;
}): string {
	if (applying > 0 && deployQueued) {
		return `Applying ${applying} ${pluralizeChange(applying)}, next deploy queued`;
	}
	if (applying > 0 && deployable > 0) {
		return `Applying ${applying} ${pluralizeChange(applying)}, ${deployable} ready`;
	}
	if (applying > 0) {
		return `Applying ${applying} ${pluralizeChange(applying)}`;
	}
	return `Apply ${total} ${pluralizeChange(total)}`;
}

function pluralizeChange(count: number): string {
	return count === 1 ? "change" : "changes";
}

function sectionSummary(
	changes: NonNullable<DashboardServiceRecord["unappliedChanges"]>,
): string {
	const counts = new Map<string, number>();
	for (const change of changes) {
		counts.set(change.section, (counts.get(change.section) ?? 0) + 1);
	}
	return Array.from(counts.entries())
		.map(([section, count]) => `${section} ${count}`)
		.join(" · ");
}

function hydrateServiceStatusSnapshot(raw: unknown): DashboardServiceStatus {
	const snapshot = raw as DashboardServiceStatus & {
		service: DashboardServiceStatus["service"] & {
			latestBuild?: DashboardServiceStatus["service"]["latestBuild"];
		};
	};
	return {
		...snapshot,
		service: hydrateServiceSnapshot(snapshot.service),
		allocation: snapshot.allocation
			? {
					...snapshot.allocation,
					updatedAt: hydrateDate(snapshot.allocation.updatedAt),
				}
			: undefined,
	};
}

function hydrateServiceSnapshots(raw: unknown): Array<DashboardServiceRecord> {
	return Array.isArray(raw) ? raw.map(hydrateServiceSnapshot) : [];
}

function hydrateServiceSnapshot(
	service: DashboardServiceRecord,
): DashboardServiceRecord {
	return {
		...service,
		createdAt: hydrateDate(service.createdAt),
		updatedAt: hydrateDate(service.updatedAt),
		latestBuild: service.latestBuild
			? {
					...service.latestBuild,
					queuedAt: hydrateDate(service.latestBuild.queuedAt),
					startedAt: hydrateDate(service.latestBuild.startedAt),
					finishedAt: hydrateDate(service.latestBuild.finishedAt),
					stages:
						service.latestBuild.stages?.map((stage) => ({
							...stage,
							startedAt: hydrateDate(stage.startedAt),
							finishedAt: hydrateDate(stage.finishedAt),
						})) ?? [],
				}
			: undefined,
	};
}

function hydrateDate(value: Date | string | undefined): Date | undefined {
	if (!value) return undefined;
	return value instanceof Date ? value : new Date(value);
}
