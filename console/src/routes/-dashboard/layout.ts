export const GRID_BOX = 128;
export const NODE_W = GRID_BOX * 2;
export const NODE_H = GRID_BOX;

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
