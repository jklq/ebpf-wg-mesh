import { Loader2, RotateCcw, UploadCloud } from "lucide-react";
import type { MouseEvent as ReactMouseEvent, RefObject } from "react";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import {
	deployActionLabel,
	dirtyPromptDetail,
	dirtyPromptTitle,
} from "./dashboard-unapplied";
import { EmptyCanvas } from "./empty-canvas";
import { NODE_H, NODE_W, nodePosition } from "./layout";
import { ServiceNode } from "./service-node";

export const CANVAS_SNAP = 32;
export const CANVAS_MAJOR_GRID = 128;
export const MIN_CANVAS_ZOOM = 0.3;
export const MAX_CANVAS_ZOOM = 2;
export const CANVAS_WHEEL_ZOOM_SENSITIVITY = 0.006;

export type Point = { x: number; y: number };

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

export function ServicePanelFallback({
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
					<span />
					<span className="skeleton-line skeleton-sha" />
				</div>
			</div>
		</div>
	);
}

export function snapToCanvas(value: number): number {
	return Math.round(value / CANVAS_SNAP) * CANVAS_SNAP;
}

export function clampCanvasZoom(value: number): number {
	return Math.min(MAX_CANVAS_ZOOM, Math.max(MIN_CANVAS_ZOOM, value));
}

export function normalizeWheelDelta(event: WheelEvent): Point {
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

export function centeredServicesPanOffset({
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

export function DashboardCanvasStage({
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
	onSelectService,
	onResetView,
	onEscape,
	panCursor,
	promptLeft,
	services,
	selectedId,
	localState,
	showNewService,
	onAddService,
	onPreloadAdd,
	showPrompt,
	changeSignature,
	deployError,
	applyingChangeCount,
	deployingChanges,
	deployableUnappliedChanges,
	hasPendingSpecWrites,
	totalUnappliedChanges,
	onShowDetails,
	onDeploy,
}: {
	canvasRef: RefObject<HTMLDivElement | null>;
	panOffset: Point;
	zoom: number;
	setZoom: (updater: (current: number) => number) => void;
	canvasReady: boolean;
	showCanvasSkeleton: boolean;
	servicePositions: Record<string, Point>;
	onCanvasMouseDown: (event: ReactMouseEvent<HTMLElement>) => void;
	onServiceMouseDown: (
		serviceId: string,
		event: ReactMouseEvent<HTMLElement>,
	) => void;
	onCanvasClick: (event: ReactMouseEvent<HTMLElement>) => void;
	onSelectService: (serviceId: string) => void;
	onResetView: () => void;
	onEscape: () => void;
	panCursor: boolean;
	promptLeft: number | undefined;
	services: Array<DashboardServiceRecord>;
	selectedId: string | null;
	localState: DashboardHomeState;
	showNewService: boolean;
	onAddService: () => void;
	onPreloadAdd: () => void;
	showPrompt: boolean;
	changeSignature: string;
	deployError?: string;
	applyingChangeCount: number;
	deployingChanges: boolean;
	deployableUnappliedChanges: number;
	hasPendingSpecWrites: boolean;
	totalUnappliedChanges: number;
	onShowDetails: () => void;
	onDeploy: () => void;
}) {
	return (
		<div
			role="application"
			tabIndex={-1}
			style={{
				flex: 1,
				marginTop: "var(--dashboard-header-height)",
				position: "relative",
				overflow: "hidden",
				cursor: panCursor ? "grabbing" : "grab",
			}}
			ref={canvasRef}
			className="canvas-grid"
			onMouseDown={onCanvasMouseDown}
			onClick={onCanvasClick}
			onKeyDown={(event) => {
				if (event.key === "Escape") onEscape();
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
						onSelect={() => onSelectService(service.id)}
					/>
				))}
			</div>

			{services.length === 0 && !showNewService && (
				<EmptyCanvas
					state={localState}
					onAdd={onAddService}
					onPreloadAdd={onPreloadAdd}
				/>
			)}

			{showPrompt && (
				<div
					key={changeSignature || "deploy-prompt"}
					className={`dirty-workspace-banner${
						deployError ? " failed" : applyingChangeCount > 0 ? " applying" : ""
					}${changeSignature ? " changed" : ""}`}
					style={
						promptLeft === undefined
							? { display: "none" }
							: { left: promptLeft }
					}
				>
					<div>
						<span className="dirty-workspace-title">
							{dirtyPromptTitle({
								applying: applyingChangeCount,
								deployError,
								deploying: deployingChanges,
							})}
						</span>
						<span className="dirty-workspace-detail">
							{dirtyPromptDetail({
								applying: applyingChangeCount,
								deployable: deployableUnappliedChanges,
								saving: hasPendingSpecWrites,
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
						onClick={onShowDetails}
					>
						Details
					</button>
					<button
						type="button"
						className="btn-primary"
						onClick={onDeploy}
						disabled={
							deployingChanges ||
							(!hasPendingSpecWrites && deployableUnappliedChanges === 0)
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
						{deployActionLabel({ deploying: deployingChanges })}
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
					onClick={onResetView}
				>
					<RotateCcw size={14} />
				</button>
			</div>
		</div>
	);
}

export function serviceLayoutPositions(
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
