import { Loader2, RefreshCw, RotateCcw, UploadCloud, X } from "lucide-react";
import type { MouseEvent as ReactMouseEvent, RefObject } from "react";
import { cn } from "#/lib/cn";
import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { btnPrimary, btnSecondary, panelIconBtn } from "#/lib/ui-classes";

import {
	deployActionLabel,
	dirtyPromptDetail,
	dirtyPromptTitle,
} from "./dashboard-unapplied";
import { EmptyCanvas } from "./empty-canvas";
import { NODE_H, NODE_W, nodePosition } from "./layout";
import { ServiceNode } from "./service-node";

const skeletonShimmerClass =
	"absolute inset-0 -translate-x-[120%] animate-skeleton-shimmer bg-gradient-to-r from-transparent via-[rgba(212,205,197,0.08)] to-transparent";

const zoomBtnClass =
	"flex size-8 cursor-pointer items-center justify-center border-0 bg-transparent font-mono text-sm text-muted hover:bg-surface-hover hover:text-ink [&:not(:first-child)]:border-t [&:not(:first-child)]:border-line";

function SkeletonShimmer() {
	return <span className={skeletonShimmerClass} />;
}

function SkeletonChip({ className }: { className?: string }) {
	return (
		<span
			className={cn(
				"relative inline-block overflow-hidden bg-[rgba(80,76,71,0.42)]",
				className,
			)}
		>
			<SkeletonShimmer />
		</span>
	);
}

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
			className={
				showTopbar ? "flex h-dvh flex-col overflow-hidden bg-canvas" : undefined
			}
			aria-hidden="true"
		>
			{showTopbar && (
				<div className="pointer-events-none fixed inset-x-0 top-0 z-40 flex h-header items-center gap-3 border-b border-line bg-surface px-4">
					<span className="relative size-[15px] shrink-0 overflow-hidden bg-[rgba(212,119,26,0.22)] [clip-path:polygon(52%_0,100%_44%,68%_44%,86%_100%,0_48%,40%_48%)]">
						<SkeletonShimmer />
					</span>
					<SkeletonChip className="h-3.5 w-12" />
					<SkeletonChip className="h-6 w-[108px] border border-line" />
					<SkeletonChip className="h-2.5 w-[66px]" />
					<span className="flex-1" />
					<SkeletonChip className="size-7 border border-line" />
					<SkeletonChip className="h-[30px] w-24 border border-[rgba(212,119,26,0.24)] bg-[rgba(212,119,26,0.08)]" />
					<SkeletonChip className="size-7 border border-line" />
				</div>
			)}
			<div
				className={cn(
					"overflow-hidden bg-canvas",
					showTopbar
						? "relative mt-header h-[calc(100dvh-var(--spacing-header))]"
						: "pointer-events-none absolute inset-0 z-[15]",
				)}
			>
				<div className="canvas-world-grid" />
				<div className="pointer-events-none absolute inset-0">
					<SkeletonNode />
				</div>
			</div>
		</div>
	);
}

export function ServicePanelFallback({
	service,
	onClose,
	onRefresh,
}: {
	service: DashboardServiceRecord;
	onClose?: () => void;
	onRefresh?: () => void;
}) {
	return (
		<>
			<div className="flex h-header shrink-0 items-center gap-1.5 border-b border-line bg-[linear-gradient(180deg,rgba(255,255,255,0.02),transparent),rgba(24,23,21,0.92)] px-4 py-2">
				<strong className="font-display text-[22px] font-medium tracking-[-0.03em] text-ink">
					{service.name}
				</strong>
				<span className="flex-1" />
				<Loader2 size={13} className="animate-spin" />
				<button
					type="button"
					className={panelIconBtn}
					onClick={onRefresh}
					disabled={!onRefresh}
					title="Refresh service"
				>
					<RefreshCw size={13} />
				</button>
				<button
					type="button"
					className={panelIconBtn}
					onClick={onClose}
					disabled={!onClose}
					title="Close service panel"
				>
					<X size={14} />
				</button>
			</div>
			<div className="relative min-h-0 flex-1 overflow-hidden">
				<div className="flex h-full flex-col gap-4 overflow-y-auto px-[18px] pt-[18px] pb-7">
					<div className="flex items-center">
						<Loader2 size={13} className="animate-spin" />
					</div>
				</div>
			</div>
		</>
	);
}

function SkeletonNode() {
	return (
		<div
			className="skeleton-node pointer-events-none absolute flex flex-col overflow-hidden rounded-sm border border-line bg-surface-raised opacity-[0.92]"
			style={{ width: NODE_W, height: NODE_H }}
		>
			<SkeletonShimmer />
			<div className="flex items-center gap-2 border-b border-line px-3.5 pt-2.5 pb-2">
				<span className="relative size-[7px] shrink-0 overflow-hidden rounded-sm bg-[rgba(212,119,26,0.22)]">
					<SkeletonShimmer />
				</span>
				<SkeletonChip className="h-[13px] w-[104px]" />
			</div>
			<div className="flex flex-1 flex-col gap-[7px] px-3.5 pt-2 pb-2.5">
				<div className="flex items-center gap-[5px]">
					<SkeletonChip className="size-2.5 shrink-0" />
					<SkeletonChip className="h-[9px] w-[86px]" />
				</div>
				<div className="flex items-center gap-[5px]">
					<SkeletonChip className="size-2.5 shrink-0" />
					<SkeletonChip className="h-[9px] w-[58px]" />
				</div>
				<div className="mt-auto flex items-center justify-between gap-1.5">
					<span />
					<SkeletonChip className="h-2.5 w-[42px]" />
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
	panelOpen,
	services,
	pendingServiceIds,
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
	panelOpen: boolean;
	services: Array<DashboardServiceRecord>;
	pendingServiceIds: ReadonlySet<string>;
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
			ref={canvasRef}
			className={cn(
				"relative mt-header flex-1 overflow-hidden bg-canvas",
				panCursor ? "cursor-grabbing" : "cursor-grab",
			)}
			onMouseDown={onCanvasMouseDown}
			onClick={onCanvasClick}
			onKeyDown={(event) => {
				if (event.key === "Escape") onEscape();
			}}
		>
			{showCanvasSkeleton && <DashboardCanvasSkeleton />}
			<div
				data-canvas-world=""
				className="absolute inset-0 origin-top-left"
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
						pending={pendingServiceIds.has(service.id)}
						onMouseDown={(event) => {
							if (pendingServiceIds.has(service.id)) {
								event.stopPropagation();
								return;
							}
							onServiceMouseDown(service.id, event);
						}}
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
					data-workspace-prompt=""
					className={cn(
						"pointer-events-none absolute top-[18px] right-0 left-0 z-[35] flex justify-center px-6 max-sm:top-3",
						panelOpen && "max-[900px]:hidden",
					)}
					style={{ right: panelOpen ? "var(--spacing-side-panel)" : 0 }}
				>
					<div
						key={changeSignature || "deploy-prompt"}
						className={cn(
							"pointer-events-auto flex w-fit min-w-[min(520px,100%)] max-w-full items-center gap-3.5 overflow-hidden border border-line-bright bg-surface bg-[linear-gradient(180deg,rgba(255,255,255,0.04),transparent_40%)] py-[11px] pr-3 pl-4 shadow-[inset_3px_0_0_var(--color-accent),0_16px_40px_rgba(0,0,0,0.4)] max-sm:flex-col max-sm:items-stretch",
							deployError &&
								"border-failed/50 shadow-[inset_3px_0_0_var(--color-failed),0_16px_40px_rgba(0,0,0,0.4)]",
							!deployError &&
								applyingChangeCount > 0 &&
								"border-building/50 shadow-[inset_3px_0_0_var(--color-building),0_16px_40px_rgba(0,0,0,0.4)]",
							changeSignature && "animate-dirty-banner-nudge",
						)}
					>
						<div className="flex min-w-0 flex-1 items-baseline gap-2 overflow-hidden max-sm:flex-wrap">
							<span className="shrink-0 font-display text-[18px] font-medium tracking-[-0.02em] text-ink">
								{dirtyPromptTitle({
									applying: applyingChangeCount,
									deployError,
									deploying: deployingChanges,
								})}
							</span>
							<span className="min-w-0 overflow-hidden font-mono text-[11px] text-ellipsis whitespace-nowrap text-muted">
								{dirtyPromptDetail({
									applying: applyingChangeCount,
									deployable: deployableUnappliedChanges,
									saving: hasPendingSpecWrites,
									total: totalUnappliedChanges,
								})}
							</span>
							{deployError && (
								<span className="min-w-0 overflow-hidden font-mono text-[11px] text-ellipsis whitespace-nowrap text-failed">
									{deployError}
								</span>
							)}
						</div>
						<button
							type="button"
							className={btnSecondary}
							onClick={onShowDetails}
						>
							Details
						</button>
						<button
							type="button"
							className={btnPrimary}
							onClick={onDeploy}
							disabled={
								deployingChanges ||
								(!hasPendingSpecWrites && deployableUnappliedChanges === 0)
							}
						>
							{deployingChanges ? (
								<Loader2 size={13} className="animate-spin" />
							) : (
								<UploadCloud size={13} />
							)}
							{deployActionLabel({ deploying: deployingChanges })}
						</button>
					</div>
				</div>
			)}

			<div className="fixed bottom-6 left-6 z-30 flex flex-col overflow-hidden rounded-sm border border-line bg-surface-raised">
				<button
					type="button"
					className={zoomBtnClass}
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
					className={zoomBtnClass}
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
					className={zoomBtnClass}
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
