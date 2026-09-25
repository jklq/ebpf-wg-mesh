// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

const fetch = vi.hoisted(() => vi.fn());
vi.mock("./server-fns", () => ({ fetchServiceLogs: fetch }));

import { DeploymentLogsView } from "./panel-deployment-logs";

afterEach(cleanup);
it("renders dropped windows, truncation, and expandable platform attributes across pages", async () => {
	fetch
		.mockResolvedValueOnce({
			lines: [
				{
					lineId: "1",
					line: "output",
					sequence: 1,
					allocationId: "",
					agentId: "",
					stream: "stdout",
					rolloutGeneration: 0,
					truncated: true,
				},
				{
					lineId: "2",
					line: "Starting",
					event: "deploy.started",
					attributes: { phase: "start" },
					sequence: 2,
					allocationId: "",
					agentId: "",
					stream: "stdout",
					rolloutGeneration: 0,
				},
			],
			gaps: [],
			nextGapPageToken: "g2",
		})
		.mockResolvedValueOnce({
			lines: [],
			gaps: [
				{
					allocationId: "",
					buildId: "",
					stream: "stdout",
					droppedCount: 42,
					reason: "buffer full",
				},
			],
		});
	render(
		<DeploymentLogsView
			service={{ id: "s", environmentId: "e", name: "web" }}
			project={{ id: "p", name: "demo", kind: "PROJECT_KIND_USER" }}
			build={undefined}
			allocation={undefined}
			active={false}
		/>,
	);
	await screen.findByText("(truncated at 64 KiB)");
	const event = screen.getByText(/Platform event · deploy.started/);
	fireEvent.click(event);
	expect(screen.getByText(/"phase": "start"/)).toBeTruthy();
	fireEvent.click(screen.getByRole("button", { name: "Load older logs" }));
	await screen.findByText(/42 lines dropped \(buffer full\)/);
	expect(screen.getByText("output")).toBeTruthy();
	expect(screen.queryByRole("button", { name: "Load older logs" })).toBeNull();
});
