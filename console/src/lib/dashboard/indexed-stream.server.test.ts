import { describe, expect, it } from "vitest";
import { indexedEventResponse } from "./indexed-stream.server";

describe("indexedEventResponse", () => {
	it("frames indexed snapshots and closes when the request is aborted", async () => {
		const request = new AbortController();
		const seenIndexes: string[] = [];
		const response = indexedEventResponse({
			event: "status",
			signal: request.signal,
			load: async (after) => {
				seenIndexes.push(after);
				if (seenIndexes.length === 1) {
					return {
						index: "9007199254740993",
						notModified: false,
						value: { serviceId: "service-1" },
					};
				}
				request.abort();
				return { index: "9007199254740993", notModified: true };
			},
		});

		expect(response.headers.get("content-type")).toBe(
			"text/event-stream; charset=utf-8",
		);
		expect(await response.text()).toBe(
			'retry: 1000\n\nid: 9007199254740993\nevent: status\ndata: {"serviceId":"service-1"}\n\n',
		);
		expect(seenIndexes).toEqual(["0", "9007199254740993"]);
	});
});
