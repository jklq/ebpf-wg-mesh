import { NODE_H, NODE_W } from "#/features/dashboard/canvas/layout";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

export const CANVAS_SNAP = 32;
export const CANVAS_MAJOR_GRID = 128;
export const MIN_CANVAS_ZOOM = 0.3;
export const MAX_CANVAS_ZOOM = 2;
export const CANVAS_WHEEL_ZOOM_SENSITIVITY = 0.006;
export type Point = { x: number; y: number };
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
export function snapPanToMajorGrid(value: number, zoom: number): number {
	const size = CANVAS_MAJOR_GRID * zoom;
	return Math.round(value / size) * size;
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
