import {
	type MouseEvent as ReactMouseEvent,
	useCallback,
	useEffect,
	useLayoutEffect,
	useMemo,
	useRef,
	useState,
} from "react";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

import {
	CANVAS_WHEEL_ZOOM_SENSITIVITY,
	centeredServicesPanOffset,
	clampCanvasZoom,
	normalizeWheelDelta,
	type Point,
	serviceLayoutPositions,
	snapToCanvas,
} from "./dashboard-canvas";
import {
	NODE_H,
	NODE_W,
	nodePosition,
	SIDE_PANEL_VIEWPORT_RATIO,
} from "./layout";
import { doSaveServicePosition } from "./server-fns";

export function useDashboardCanvas({
	services,
	environmentId,
	selectedId,
	selected,
	onDeselect,
}: {
	services: Array<DashboardServiceRecord>;
	environmentId: string | null;
	selectedId: string | null;
	selected: DashboardServiceRecord | undefined;
	onDeselect: () => void;
}) {
	const [panOffset, setPanOffset] = useState<Point>({ x: 0, y: 0 });
	const [canvasSize, setCanvasSize] = useState({ width: 0, height: 0 });
	const [canvasReady, setCanvasReady] = useState(false);
	const targetPanOffset = useRef<Point>({ x: 0, y: 0 });
	const panAnimationFrame = useRef<number | null>(null);
	const [zoom, setZoom] = useState(1);
	const zoomRef = useRef(zoom);
	const [nodePositions, setNodePositions] = useState<Record<string, Point>>(
		() => serviceLayoutPositions(services),
	);
	const [promptLeft, setPromptLeft] = useState<number>();
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
	const centeredEnvironmentId = useRef<string | null | undefined>(undefined);
	const canvasRef = useRef<HTMLDivElement>(null);

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

	useEffect(() => {
		panOffsetRef.current = panOffset;
	}, [panOffset]);

	useEffect(() => {
		zoomRef.current = zoom;
	}, [zoom]);

	useEffect(() => {
		nodePositionsRef.current = nodePositions;
	}, [nodePositions]);

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
			? canvas.clientWidth * (1 - SIDE_PANEL_VIEWPORT_RATIO)
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
			const sidePanelWidth = selected
				? window.innerWidth * SIDE_PANEL_VIEWPORT_RATIO
				: 0;
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
			onDeselect();
		}
	};

	const selectService = (id: string) => {
		if (isDragging.current || suppressNextClick.current) return;
		hasUserPanned.current = false;
		return id;
	};

	const handleResetView = () => {
		const next = { x: 0, y: 0 };
		setZoom(1);
		setTargetPanOffset(next);
		hasUserPanned.current = false;
	};

	const showCanvasSkeleton = services.length > 0 && !canvasReady;

	return {
		canvasRef,
		panOffset,
		zoom,
		setZoom,
		canvasReady,
		showCanvasSkeleton,
		servicePositions,
		onCanvasMouseDown,
		onServiceMouseDown,
		onCanvasClick,
		selectService,
		handleResetView,
		setNodePositions,
		nodePositionsRef,
		hasUserPanned,
		promptLeft,
		panStart,
	};
}
