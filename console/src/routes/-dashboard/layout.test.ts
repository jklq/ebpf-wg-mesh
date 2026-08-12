import { describe, expect, it } from "vitest";

import { nextNodePositionNear, nodePosition } from "./layout";

describe("dashboard layout", () => {
	it("places services on the nearest available grid slot", () => {
		expect(nextNodePositionNear([])).toEqual(nodePosition(0));
		expect(
			nextNodePositionNear([
				{ x: 128, y: 128 },
				{ x: 512, y: 128 },
				{ x: 896, y: 128 },
			]),
		).toEqual({ x: 128, y: 384 });
		expect(nextNodePositionNear([{ x: 640, y: 384 }])).toEqual({
			x: 1024,
			y: 384,
		});
	});
});
