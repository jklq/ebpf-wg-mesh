export const NODE_W = 224;
export const NODE_H = 130;

const COL_GAP = 300;
const ROW_GAP = 180;
const COLS = 3;
const ORIGIN_X = 80;
const ORIGIN_Y = 80;

export function nodePosition(index: number): { x: number; y: number } {
	return {
		x: ORIGIN_X + (index % COLS) * COL_GAP,
		y: ORIGIN_Y + Math.floor(index / COLS) * ROW_GAP,
	};
}
