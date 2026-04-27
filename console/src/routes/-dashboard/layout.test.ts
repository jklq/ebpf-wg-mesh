import { describe, expect, it } from "vitest";

import { nextNodePositionNear, nodePosition } from "./layout";

describe("dashboard layout", () => {
	it("starts the first service on the grid", () => {
		expect(nextNodePositionNear([])).toEqual(nodePosition(0));
	});

	it("places new services in the nearest open grid slot around existing services", () => {
		expect(
			nextNodePositionNear([
				{ x: 128, y: 128 },
				{ x: 512, y: 128 },
				{ x: 896, y: 128 },
			]),
		).toEqual({ x: 128, y: 384 });
	});

	it("anchors placement near custom-positioned services", () => {
		expect(nextNodePositionNear([{ x: 640, y: 384 }])).toEqual({
			x: 1024,
			y: 384,
		});
	});
});
