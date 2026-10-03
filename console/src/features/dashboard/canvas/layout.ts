export const GRID_BOX = 128;
export const NODE_W = GRID_BOX * 2;
export const NODE_H = GRID_BOX;
/** Must match sizes.sidePanel in styles/tokens.stylex.ts. */
export const SIDE_PANEL_VIEWPORT_RATIO = 0.4;

const COL_GAP = GRID_BOX * 3;
const ROW_GAP = GRID_BOX * 2;
const COLS = 3;
const ORIGIN_X = GRID_BOX;
const ORIGIN_Y = GRID_BOX;

export function nodePosition(index: number): { x: number; y: number } {
	return {
		x: ORIGIN_X + (index % COLS) * COL_GAP,
		y: ORIGIN_Y + Math.floor(index / COLS) * ROW_GAP,
	};
}

export function nextNodePositionNear(
	existingPositions: Array<{ x: number; y: number }>,
): { x: number; y: number } {
	if (existingPositions.length === 0) {
		return nodePosition(0);
	}

	const anchor = {
		x: snapToGrid(Math.min(...existingPositions.map((position) => position.x))),
		y: snapToGrid(Math.min(...existingPositions.map((position) => position.y))),
	};

	for (let index = 0; index < 10_000; index += 1) {
		const candidate = {
			x: anchor.x + (index % COLS) * COL_GAP,
			y: anchor.y + Math.floor(index / COLS) * ROW_GAP,
		};
		if (
			!existingPositions.some((position) => nodesOverlap(candidate, position))
		) {
			return candidate;
		}
	}

	return nodePosition(existingPositions.length);
}

function snapToGrid(value: number): number {
	return Math.round(value / GRID_BOX) * GRID_BOX;
}

function nodesOverlap(
	a: { x: number; y: number },
	b: { x: number; y: number },
): boolean {
	return (
		a.x < b.x + NODE_W &&
		a.x + NODE_W > b.x &&
		a.y < b.y + NODE_H &&
		a.y + NODE_H > b.y
	);
}

/** Height of the volume strip attached beneath a service card. */
export const VOLUME_TAB_H = 40;
/** Height of a standalone, unmounted volume card. */
export const VOLUME_NODE_H = 48;

/**
 * Unmounted volumes line up beneath the services. They have no stored
 * position: once mounted they travel with their service's card.
 */
export function unattachedVolumePositions(
	servicePositions: Array<{ x: number; y: number }>,
	count: number,
): Array<{ x: number; y: number }> {
	const originX =
		servicePositions.length > 0
			? snapToGrid(Math.min(...servicePositions.map((position) => position.x)))
			: ORIGIN_X;
	const bottom =
		servicePositions.length > 0
			? Math.max(...servicePositions.map((position) => position.y)) +
				NODE_H +
				VOLUME_TAB_H
			: ORIGIN_Y - GRID_BOX;
	const y = Math.ceil((bottom + GRID_BOX / 2) / GRID_BOX) * GRID_BOX;
	return Array.from({ length: count }, (_, index) => ({
		x: originX + (index % COLS) * COL_GAP,
		y: y + Math.floor(index / COLS) * GRID_BOX,
	}));
}
