import * as stylex from "@stylexjs/stylex";
import { RotateCcw } from "lucide-react";
import type { ReactNode } from "react";
import { clampCanvasZoom } from "#/features/dashboard/canvas/canvas-math";
import { DashboardCanvasSkeleton } from "#/features/dashboard/canvas/canvas-skeleton";
import { EmptyCanvas } from "#/features/dashboard/canvas/empty-canvas";
import {
	nodePosition,
	unattachedVolumePositions,
} from "#/features/dashboard/canvas/layout";
import { ServiceNode } from "#/features/dashboard/canvas/service-node";
import { VolumeNode, VolumeTab } from "#/features/dashboard/canvas/volume-node";
import {
	serviceVolume,
	unattachedVolumes,
} from "#/features/dashboard/volumes/volume-model";
import type {
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { colors, fonts, shape, sizes } from "#/styles/tokens.stylex";
import type { useDashboardCanvas } from "./use-dashboard-canvas";

type CanvasStageModel = Pick<
	ReturnType<typeof useDashboardCanvas>,
	| "canvasRef"
	| "panOffset"
	| "zoom"
	| "setZoom"
	| "canvasReady"
	| "showCanvasSkeleton"
	| "servicePositions"
	| "onCanvasMouseDown"
	| "onServiceMouseDown"
	| "onCanvasClick"
	| "handleResetView"
	| "panStart"
> & {
	services: DashboardServiceRecord[];
	pendingServiceIds: ReadonlySet<string>;
	selectedId: string | null;
	volumes: DashboardVolume[];
	selectedVolumeId: string | null;
};
export function DashboardCanvasStage({
	canvas,
	localState,
	showNewService,
	onAddService,
	onPreloadAdd,
	onSelectService,
	onSelectVolume,
	onMountVolume,
	onEscape,
	children,
}: {
	canvas: CanvasStageModel;
	localState: DashboardHomeState;
	showNewService: boolean;
	onAddService: () => void;
	onPreloadAdd: () => void;
	onSelectService: (id: string) => void;
	onSelectVolume: (id: string) => void;
	onMountVolume: (id: string) => void;
	onEscape: () => void;
	children?: ReactNode;
}) {
	const {
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
		handleResetView: onResetView,
		services,
		pendingServiceIds,
		selectedId,
		volumes,
		selectedVolumeId,
	} = canvas;
	const loose = unattachedVolumes(volumes, services);
	const loosePositions = unattachedVolumePositions(
		services.map(
			(service, index) => servicePositions[service.id] ?? nodePosition(index),
		),
		loose.length,
	);
	const panCursor = Boolean(canvas.panStart.current);
	return (
		<div
			role="application"
			tabIndex={-1}
			ref={canvasRef}
			{...stylex.props([
				styles.canvas,
				panCursor ? styles.panning : styles.idleCursor,
			])}
			onMouseDown={onCanvasMouseDown}
			onClick={onCanvasClick}
			onKeyDown={(event) => {
				if (event.key === "Escape") onEscape();
			}}
		>
			{showCanvasSkeleton && <DashboardCanvasSkeleton />}
			<div
				data-canvas-world=""
				{...stylex.props(
					styles.world,
					styles.worldTransform(panOffset.x, panOffset.y, zoom),
					styles.worldVisibility(
						canvasReady || services.length === 0 ? "visible" : "hidden",
					),
				)}
			>
				<div {...stylex.props(styles.worldGrid)} />
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
				{services.map((service, index) => {
					const volume = serviceVolume(service, volumes);
					if (!volume) return null;
					return (
						<VolumeTab
							key={`volume-${volume.id}`}
							volume={volume}
							servicePos={servicePositions[service.id] ?? nodePosition(index)}
							selected={volume.id === selectedVolumeId}
							onSelect={() => onSelectVolume(volume.id)}
						/>
					);
				})}
				{loose.map((volume, index) => (
					<VolumeNode
						key={`volume-${volume.id}`}
						volume={volume}
						pos={loosePositions[index] ?? nodePosition(index)}
						selected={volume.id === selectedVolumeId}
						onSelect={() => onSelectVolume(volume.id)}
						onMount={() => onMountVolume(volume.id)}
					/>
				))}
			</div>

			{services.length === 0 && loose.length === 0 && !showNewService && (
				<EmptyCanvas
					state={localState}
					onAdd={onAddService}
					onPreloadAdd={onPreloadAdd}
				/>
			)}

			{children}

			<div {...stylex.props(styles.zoomControls)}>
				<button
					type="button"
					{...stylex.props(styles.zoomButton)}
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
					{...stylex.props(styles.zoomButton)}
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
					{...stylex.props(styles.zoomButton)}
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

const styles = stylex.create({
	worldTransform: (x: number, y: number, zoom: number) => ({
		transform: `translate(${x}px,${y}px) scale(${zoom})`,
	}),
	worldVisibility: (value: "visible" | "hidden") => ({ visibility: value }),
	zoomButton: {
		display: "flex",
		width: "2rem",
		height: "2rem",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		fontFamily: fonts.mono,
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
		borderTopStyle: { default: null, ":not(:first-child)": "solid" },
		borderTopWidth: { default: null, ":not(:first-child)": "1px" },
		borderColor: { default: null, ":not(:first-child)": colors.line },
	},
	worldGrid: {
		position: "absolute",
		left: "-8192px",
		top: "-8192px",
		width: "16384px",
		height: "16384px",
		pointerEvents: "none",
		backgroundImage:
			"linear-gradient(rgba(54, 50, 47, 0.65) 1px, transparent 1px), linear-gradient(90deg, rgba(54, 50, 47, 0.65) 1px, transparent 1px), linear-gradient(rgba(38, 36, 33, 0.38) 1px, transparent 1px), linear-gradient(90deg, rgba(38, 36, 33, 0.38) 1px, transparent 1px)",
		backgroundSize: "128px 128px, 128px 128px, 32px 32px, 32px 32px",
	},
	canvas: {
		position: "relative",
		marginTop: sizes.header,
		flex: "1",
		overflow: "hidden",
		backgroundColor: colors.canvas,
	},
	panning: { cursor: "grabbing" },
	idleCursor: { cursor: "grab" },
	world: { position: "absolute", inset: "0rem", transformOrigin: "0 0" },
	zoomControls: {
		position: "fixed",
		bottom: "1.5rem",
		left: "1.5rem",
		zIndex: "30",
		display: "flex",
		flexDirection: "column",
		overflow: "hidden",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
	},
});
