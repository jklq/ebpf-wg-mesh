import { useRouter } from "@tanstack/react-router";
import { Loader2, RotateCcw, Trash2, UploadCloud, X } from "lucide-react";
import {
	type MouseEvent as ReactMouseEvent,
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
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { EmptyCanvas } from "./empty-canvas";
import { NODE_H, NODE_W, nextNodePositionNear, nodePosition } from "./layout";
import { NewServiceModal } from "./new-service-modal";
import {
	doDiscardServiceChanges,
	doRedeployService,
	doSaveServicePosition,
} from "./server-fns";
import { ServiceNode } from "./service-node";
import { ServicePanel } from "./service-panel";
import { formatError, serviceHealth } from "./service-utils";
import { Topbar } from "./topbar";
import type { DashboardTab } from "./types";

const CANVAS_SNAP = 32;

type Point = { x: number; y: number };

export function DashboardPage({ state }: { state: DashboardHomeState }) {
	const router = useRouter();
	const [localState, setLocalState] = useState(state);
	const services = localState.services;
	const [selectedId, setSelectedId] = useState<string | null>(null);
	const [activeTab, setActiveTab] = useState<DashboardTab>("deployments");
	const [liveStatus, setLiveStatus] = useState<DashboardServiceStatus | null>(
		null,
	);
	const [, setStatusLoading] = useState(false);
	const [showNewService, setShowNewService] = useState(false);
	const [deployingChanges, setDeployingChanges] = useState(false);
	const [deployError, setDeployError] = useState<string>();
	const [showChangeDetails, setShowChangeDetails] = useState(false);
	const [discardingChangeId, setDiscardingChangeId] = useState<string>();
	const [promptLeft, setPromptLeft] = useState<number>();
	const [promptHidden, setPromptHidden] = useState(false);
	const [panOffset, setPanOffset] = useState<Point>({ x: 0, y: 0 });
	const [canvasReady, setCanvasReady] = useState(false);
	const targetPanOffset = useRef<Point>({ x: 0, y: 0 });
	const [zoom, setZoom] = useState(1);
	const [nodePositions, setNodePositions] = useState<Record<string, Point>>(
		() => serviceLayoutPositions(state.services),
	);
	const [, startTransition] = useTransition();
	const projectId = localState.project?.id ?? null;
	const panOffsetRef = useRef(panOffset);
	const nodePositionsRef = useRef(nodePositions);
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
	const centeredProjectId = useRef<string | null | undefined>(undefined);
	const canvasRef = useRef<HTMLDivElement>(null);
	const selected = services.find((service) => service.id === selectedId);
	const dirtyServices = services.filter(hasUnappliedChanges);
	const totalUnappliedChanges = dirtyServices.reduce(
		(total, service) => total + unappliedChangeCount(service),
		0,
	);
	const showPrompt =
		dirtyServices.length > 0 && (!promptHidden || Boolean(deployError));

	useEffect(() => {
		panOffsetRef.current = panOffset;
	}, [panOffset]);

	useEffect(() => {
		nodePositionsRef.current = nodePositions;
	}, [nodePositions]);

	const persistNodePositions = useCallback(
		(serviceId: string, position: Point) => {
			if (!projectId) return;
			void doSaveServicePosition({
				data: { projectId, serviceId, position },
			});
		},
		[projectId],
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
			services.length === 0 ||
			selectedId ||
			hasUserPanned.current ||
			(centeredProjectId.current === projectId && canvasReady)
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
		centeredProjectId.current = projectId;
		targetPanOffset.current = next;
		setPanOffset(next);
		setCanvasReady(true);
	}, [canvasReady, projectId, selectedId, servicePositions, services, zoom]);

	useEffect(() => {
		if (!selectedId || !canvasRef.current || hasUserPanned.current) return;
		const pos = servicePositions[selectedId];
		if (!pos) return;
		const canvas = canvasRef.current;
		const sidePanelOpen = Boolean(selected);
		const visibleWidth = sidePanelOpen
			? canvas.clientWidth * 0.4
			: canvas.clientWidth;
		targetPanOffset.current = {
			x: (visibleWidth - NODE_W) / 2 - pos.x,
			y: (canvas.clientHeight - NODE_H) / 2 - pos.y,
		};
	}, [selectedId, servicePositions, selected]);

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
		let rafId: number;
		const animate = () => {
			setPanOffset((prev) => {
				const tx = targetPanOffset.current.x;
				const ty = targetPanOffset.current.y;
				const dx = tx - prev.x;
				const dy = ty - prev.y;
				if (Math.abs(dx) < 0.5 && Math.abs(dy) < 0.5) {
					if (prev.x === tx && prev.y === ty) return prev;
					return { x: tx, y: ty };
				}
				return {
					x: prev.x + dx * 0.12,
					y: prev.y + dy * 0.12,
				};
			});
			rafId = requestAnimationFrame(animate);
		};
		rafId = requestAnimationFrame(animate);
		return () => cancelAnimationFrame(rafId);
	}, []);

	const mergeStatusService = useCallback((status: DashboardServiceStatus) => {
		setLocalState((current) => ({
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
		}));
	}, []);

	useEffect(() => {
		setLocalState(state);
		setStatusLoading(false);
	}, [state]);

	useEffect(() => {
		if (totalUnappliedChanges === 0) {
			setPromptHidden(false);
			setDeployError(undefined);
		}
	}, [totalUnappliedChanges]);

	useEffect(() => {
		const hasPending =
			services.some((service) => {
				const health = serviceHealth(service);
				return health === "building" || hasActiveStages(service);
			}) || hasAllocationRolloutMismatch(liveStatus);
		if (!hasPending) return;
		const id = window.setInterval(() => {
			void router.invalidate();
		}, 5000);
		return () => window.clearInterval(id);
	}, [router, services, liveStatus]);

	useEffect(() => {
		if (!selectedId || !projectId) {
			setLiveStatus(null);
			setStatusLoading(false);
			return;
		}
		setStatusLoading(true);
		const source = new EventSource(
			`/events/service-status?projectId=${encodeURIComponent(projectId)}&serviceId=${encodeURIComponent(selectedId)}`,
		);
		source.addEventListener("status", (event) => {
			const nextStatus = hydrateServiceStatusSnapshot(
				JSON.parse((event as MessageEvent<string>).data),
			);
			setLiveStatus(nextStatus);
			mergeStatusService(nextStatus);
			setStatusLoading(false);
		});
		source.addEventListener("status-error", () => {
			setStatusLoading(false);
		});
		source.onerror = () => {
			setStatusLoading(false);
		};
		return () => source.close();
	}, [selectedId, projectId, mergeStatusService]);

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
			if (result.project.id) {
				void doSaveServicePosition({
					data: {
						projectId: result.project.id,
						serviceId: result.service.id,
						position: spawnPosition,
					},
				});
			}
		}

		setLocalState((current) => {
			const nextServices = current.services.some(
				(service) => service.id === result.service.id,
			)
				? current.services.map((service) =>
						service.id === result.service.id ? result.service : service,
					)
				: [...current.services, result.service];
			return {
				...current,
				project:
					current.project?.id === result.project.id
						? current.project
						: result.project,
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
		startTransition(() => void router.invalidate());
	};

	const handleResetView = () => {
		const next = { x: 0, y: 0 };
		setZoom(1);
		setPanOffset(next);
		targetPanOffset.current = next;
		hasUserPanned.current = false;
	};

	const handleDeployChanges = async () => {
		if (!projectId || deployingChanges || dirtyServices.length === 0) return;
		setDeployError(undefined);
		setPromptHidden(true);
		setShowChangeDetails(false);
		setDeployingChanges(true);
		try {
			for (const service of dirtyServices) {
				const status = await doRedeployService({
					data: { projectId, serviceId: service.id },
				});
				mergeStatusService(status);
				if (selectedId === service.id) {
					setLiveStatus(status);
				}
			}
			startTransition(() => void router.invalidate());
		} catch (error) {
			setDeployError(`Deploy stopped: ${formatError(error)}`);
			setPromptHidden(false);
		} finally {
			setDeployingChanges(false);
		}
	};

	const handleDiscardServiceChanges = async (serviceId: string) => {
		if (!projectId || discardingChangeId) return;
		setDiscardingChangeId(`service:${serviceId}`);
		try {
			const service = await doDiscardServiceChanges({
				data: { projectId, serviceId, discardAll: true },
			});
			mergeService(service);
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
			setPromptHidden(false);
		} finally {
			setDiscardingChangeId(undefined);
		}
	};

	const handleDiscardChange = async (serviceId: string, changeId: string) => {
		if (!projectId || discardingChangeId) return;
		setDiscardingChangeId(`${serviceId}:${changeId}`);
		try {
			const service = await doDiscardServiceChanges({
				data: { projectId, serviceId, changeIds: [changeId] },
			});
			mergeService(service);
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
			setPromptHidden(false);
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
				onNewService={() => setShowNewService(true)}
				onRefresh={handleRefresh}
			/>
			<div
				role="application"
				tabIndex={-1}
				style={{
					flex: 1,
					marginTop: 48,
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
						setSelectedId(null);
						setLiveStatus(null);
					}
				}}
			>
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
						onAdd={() => setShowNewService(true)}
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
								Apply {totalUnappliedChanges}{" "}
								{totalUnappliedChanges === 1 ? "change" : "changes"}
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
							disabled={deployingChanges}
						>
							{deployingChanges ? (
								<Loader2
									size={13}
									style={{ animation: "spin 1s linear infinite" }}
								/>
							) : (
								<UploadCloud size={13} />
							)}
							{deployingChanges ? "Deploying..." : "Deploy"}
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
							setZoom((current) => Math.max(0.3, +(current - 0.1).toFixed(1)))
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
							setZoom((current) => Math.min(2, +(current + 0.1).toFixed(1)))
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
					<ServicePanel
						service={selected}
						status={liveStatus}
						project={localState.project}
						state={localState}
						activeTab={activeTab}
						onTabChange={setActiveTab}
						onClose={() => {
							setSelectedId(null);
							setLiveStatus(null);
						}}
						onRefresh={handleRefresh}
						onServiceUpdated={mergeService}
					/>
				)}
			</div>

			{showNewService && (
				<NewServiceModal
					state={localState}
					onClose={() => setShowNewService(false)}
					onCreated={handleCreated}
				/>
			)}

			{showChangeDetails && (
				<UnappliedChangesDialog
					services={dirtyServices}
					totalChanges={totalUnappliedChanges}
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

function UnappliedChangesDialog({
	services,
	totalChanges,
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
		<div className="modal-overlay" role="dialog" aria-modal="true">
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
						{affectedServices} {affectedServices === 1 ? "service" : "services"}{" "}
						affected
					</span>
					{deployError && (
						<span className="dirty-workspace-error">{deployError}</span>
					)}
					<button
						type="button"
						className="btn-primary"
						onClick={onDeploy}
						disabled={deploying}
					>
						{deploying ? (
							<Loader2
								size={13}
								style={{ animation: "spin 1s linear infinite" }}
							/>
						) : (
							<UploadCloud size={13} />
						)}
						Deploy Changes
					</button>
				</div>
			</div>
		</div>
	);
}

function snapToCanvas(value: number): number {
	return Math.round(value / CANVAS_SNAP) * CANVAS_SNAP;
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
		x: canvas.clientWidth / 2 - worldCenterX * zoom,
		y: canvas.clientHeight / 2 - worldCenterY * zoom,
	};
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

function hasActiveStages(service: DashboardServiceRecord): boolean {
	return (
		service.latestBuild?.stages?.some(
			(stage) => stage.state === "running" || stage.state === "pending",
		) ?? false
	);
}

function hasAllocationRolloutMismatch(
	status: DashboardServiceStatus | null,
): boolean {
	const allocation = status?.allocation;
	return Boolean(
		allocation &&
			allocation.desiredRolloutGeneration !==
				allocation.appliedRolloutGeneration,
	);
}

function unappliedChangeCount(service: DashboardServiceRecord): number {
	return service.unappliedChangeCount ?? (service.pendingChanges ? 1 : 0);
}

function hasUnappliedChanges(service: DashboardServiceRecord): boolean {
	return unappliedChangeCount(service) > 0;
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
		service: {
			...snapshot.service,
			createdAt: hydrateDate(snapshot.service.createdAt),
			updatedAt: hydrateDate(snapshot.service.updatedAt),
			latestBuild: snapshot.service.latestBuild
				? {
						...snapshot.service.latestBuild,
						queuedAt: hydrateDate(snapshot.service.latestBuild.queuedAt),
						startedAt: hydrateDate(snapshot.service.latestBuild.startedAt),
						finishedAt: hydrateDate(snapshot.service.latestBuild.finishedAt),
						stages:
							snapshot.service.latestBuild.stages?.map((stage) => ({
								...stage,
								startedAt: hydrateDate(stage.startedAt),
								finishedAt: hydrateDate(stage.finishedAt),
							})) ?? [],
					}
				: undefined,
		},
		allocation: snapshot.allocation
			? {
					...snapshot.allocation,
					updatedAt: hydrateDate(snapshot.allocation.updatedAt),
				}
			: undefined,
	};
}

function hydrateDate(value: Date | string | undefined): Date | undefined {
	if (!value) return undefined;
	return value instanceof Date ? value : new Date(value);
}
