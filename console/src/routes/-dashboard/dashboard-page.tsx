import { useRouter } from "@tanstack/react-router";
import { RotateCcw } from "lucide-react";
import {
	type MouseEvent as ReactMouseEvent,
	useCallback,
	useEffect,
	useRef,
	useState,
	useTransition,
} from "react";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { EmptyCanvas } from "./empty-canvas";
import { nodePosition } from "./layout";
import { NewServiceModal } from "./new-service-modal";
import { fetchServiceStatus } from "./server-fns";
import { ServiceNode } from "./service-node";
import { ServicePanel } from "./service-panel";
import { serviceHealth } from "./service-utils";
import { Topbar } from "./topbar";
import type { DashboardTab } from "./types";

export function DashboardPage({ state }: { state: DashboardHomeState }) {
	const router = useRouter();
	const [localState, setLocalState] = useState(state);
	const services = localState.services;
	const [selectedId, setSelectedId] = useState<string | null>(null);
	const [activeTab, setActiveTab] = useState<DashboardTab>("deployments");
	const [liveStatus, setLiveStatus] = useState<DashboardServiceStatus | null>(
		null,
	);
	const [statusLoading, setStatusLoading] = useState(false);
	const [showNewService, setShowNewService] = useState(false);
	const [panOffset, setPanOffset] = useState({ x: 0, y: 0 });
	const [zoom, setZoom] = useState(1);
	const [, startTransition] = useTransition();
	const panStart = useRef<{
		mx: number;
		my: number;
		ox: number;
		oy: number;
	} | null>(null);
	const isDragging = useRef(false);
	const selected = services.find((service) => service.id === selectedId);

	useEffect(() => {
		setLocalState(state);
	}, [state]);

	useEffect(() => {
		const hasPending = services.some((service) => {
			const health = serviceHealth(service);
			return health === "building";
		});
		if (!hasPending) return;
		const id = window.setInterval(() => {
			void router.invalidate();
		}, 5000);
		return () => window.clearInterval(id);
	}, [router, services]);

	useEffect(() => {
		if (!selectedId || !localState.project) {
			setLiveStatus(null);
			return;
		}
		setStatusLoading(true);
		fetchServiceStatus({
			data: { projectId: localState.project.id, serviceId: selectedId },
		})
			.then(setLiveStatus)
			.catch(() => setLiveStatus(null))
			.finally(() => setStatusLoading(false));
	}, [selectedId, localState.project]);

	const onCanvasMouseDown = useCallback(
		(event: ReactMouseEvent<HTMLElement>) => {
			const el = event.target as HTMLElement;
			if (
				el.closest(
					".service-node, button, a, input, select, textarea, [role='button']",
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

	useEffect(() => {
		const onMove = (event: globalThis.MouseEvent) => {
			if (!panStart.current) return;
			const dx = event.clientX - panStart.current.mx;
			const dy = event.clientY - panStart.current.my;
			if (Math.abs(dx) + Math.abs(dy) > 4) isDragging.current = true;
			setPanOffset({
				x: panStart.current.ox + dx,
				y: panStart.current.oy + dy,
			});
		};
		const onUp = () => {
			panStart.current = null;
		};
		window.addEventListener("mousemove", onMove);
		window.addEventListener("mouseup", onUp);
		return () => {
			window.removeEventListener("mousemove", onMove);
			window.removeEventListener("mouseup", onUp);
		};
	}, []);

	const onCanvasClick = (event: ReactMouseEvent<HTMLElement>) => {
		if (isDragging.current) return;
		const el = event.target as HTMLElement;
		if (el.closest("button, a, input, select, textarea, [role='button']")) {
			return;
		}
		if (!el.closest(".service-node")) {
			setSelectedId(null);
			setLiveStatus(null);
		}
	};

	const selectService = (id: string) => {
		if (isDragging.current) return;
		setSelectedId(id);
		setActiveTab("deployments");
		setLiveStatus(null);
	};

	const handleRefresh = () => {
		startTransition(() => {
			void router.invalidate();
		});
		if (selectedId && localState.project) {
			setStatusLoading(true);
			fetchServiceStatus({
				data: { projectId: localState.project.id, serviceId: selectedId },
			})
				.then(setLiveStatus)
				.catch(() => {})
				.finally(() => setStatusLoading(false));
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
					style={{
						position: "absolute",
						inset: 0,
						transform: `translate(${panOffset.x}px,${panOffset.y}px) scale(${zoom})`,
						transformOrigin: "0 0",
					}}
				>
					{services.map((service, index) => (
						<ServiceNode
							key={service.id}
							service={service}
							pos={nodePosition(index)}
							selected={service.id === selectedId}
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
						onClick={() => {
							setZoom(1);
							setPanOffset({ x: 0, y: 0 });
						}}
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
						statusLoading={statusLoading}
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
					onCreated={(nextState) => {
						if (nextState) {
							setLocalState(nextState);
							const created = nextState.service ?? nextState.services.at(-1);
							if (created) {
								setSelectedId(created.id);
								setActiveTab("deployments");
							}
						}
						setShowNewService(false);
						startTransition(() => void router.invalidate());
					}}
				/>
			)}
		</div>
	);
}
