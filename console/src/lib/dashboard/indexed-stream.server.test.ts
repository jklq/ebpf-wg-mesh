import { describe, expect, it } from "vitest";
import { indexedEventResponse } from "./indexed-stream.server";

describe("indexedEventResponse", () => {
	it("frames indexed snapshots and closes when the request is aborted", async () => {
		const request = new AbortController();
		const seenIndexes: number[] = [];
		const response = indexedEventResponse({
			event: "status",
			signal: request.signal,
			load: async (after) => {
				seenIndexes.push(after);
				if (seenIndexes.length === 1) {
					return {
						index: 7,
						notModified: false,
						value: { serviceId: "service-1" },
					};
				}
				request.abort();
				return { index: 7, notModified: true };
			},
		});

		expect(response.headers.get("content-type")).toBe(
			"text/event-stream; charset=utf-8",
		);
		expect(await response.text()).toBe(
			'retry: 1000\n\nid: 7\nevent: status\ndata: {"serviceId":"service-1"}\n\n',
		);
		expect(seenIndexes).toEqual([0, 7]);
	});
});
