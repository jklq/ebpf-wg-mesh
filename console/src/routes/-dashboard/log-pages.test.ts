import { expect, it, vi } from "vitest";

const fetch = vi.hoisted(() => vi.fn());
vi.mock("./server-fns", () => ({ fetchServiceLogs: fetch }));

import { fetchLogPage } from "./log-pages";

it("advances line and gap cursors independently without repeating exhausted streams", async () => {
	fetch
		.mockReset()
		.mockResolvedValueOnce({
			lines: [{ line: "one" }],
			nextPageToken: "line-2",
			gaps: [{ reason: "overflow" }],
			nextGapPageToken: "gap-2",
		})
		.mockResolvedValueOnce({
			lines: [{ line: "two" }],
			gaps: [{ reason: "offline" }],
			nextGapPageToken: "gap-3",
		})
		.mockResolvedValueOnce({
			lines: [{ line: "one" }],
			gaps: [{ reason: "full" }],
		});
	const first = await fetchLogPage({ serviceId: "s" });
	const second = await fetchLogPage({ serviceId: "s" }, first);
	const third = await fetchLogPage({ serviceId: "s" }, second);
	const result = {
		lines: [...first.lines, ...second.lines, ...third.lines],
		gaps: [...first.gaps, ...second.gaps, ...third.gaps],
	};
	expect(result.lines.map((line) => line.line)).toEqual(["one", "two"]);
	expect(result.gaps.map((gap) => gap.reason)).toEqual([
		"overflow",
		"offline",
		"full",
	]);
	expect(
		fetch.mock.calls.map((call) => [
			call[0].data.pageToken,
			call[0].data.gapPageToken,
		]),
	).toEqual([
		[undefined, undefined],
		["line-2", "gap-2"],
		[undefined, "gap-3"],
	]);
});
