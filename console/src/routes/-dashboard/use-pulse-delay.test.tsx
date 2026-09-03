// @vitest-environment jsdom

import { cleanup, render, waitFor } from "@testing-library/react";
import { act } from "react";
import { hydrateRoot } from "react-dom/client";
import { renderToString } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

import { ServiceNode } from "./service-node";
import { usePulseDelay } from "./use-pulse-delay";

afterEach(cleanup);

function buildingService(): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "hello",
		spec: {
			source: {
				provider: "github",
				repositorySelector: "octocat/hello",
				trackedRef: "main",
			},
			runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		},
		latestBuild: {
			buildId: "build-1",
			state: "running",
			commitSha: "abc1234",
			imageDigest: "",
			failureReason: "",
			stages: [
				{
					key: "build",
					label: "Build",
					detail: "",
					state: "running",
				},
			],
		},
	};
}

function PulseProbe({ active }: { active: boolean }) {
	const delay = usePulseDelay(active);
	return <span data-testid="pulse">{delay}</span>;
}

describe("usePulseDelay", () => {
	it("keeps the first render at 0ms so SSR matches hydration", () => {
		const now = vi.spyOn(Date, "now").mockReturnValue(965);
		const html = renderToString(<PulseProbe active />);
		expect(html).toContain(">0ms<");
		expect(html).not.toContain(">-965ms<");
		now.mockRestore();
	});

	it("applies the wall-clock phase after mount", async () => {
		const now = vi.spyOn(Date, "now").mockReturnValue(965);
		const { getByTestId } = render(<PulseProbe active />);
		await waitFor(() => {
			expect(getByTestId("pulse").textContent).toBe("-965ms");
		});
		now.mockRestore();
	});
});

describe("ServiceNode pulse delay", () => {
	it("does not bake Date.now() into SSR HTML", () => {
		const now = vi.spyOn(Date, "now").mockReturnValue(965);
		const html = renderToString(
			<ServiceNode
				service={buildingService()}
				pos={{ x: 128, y: 128 }}
				selected={false}
				onMouseDown={() => {}}
				onSelect={() => {}}
			/>,
		);
		expect(html).toContain("animate-pulse-building");
		expect(html).toMatch(/animation-delay:0ms/);
		expect(html).not.toMatch(/animation-delay:-965ms/);
		now.mockRestore();
	});

	it("hydrates after a different client clock without a mismatch", async () => {
		const now = vi.spyOn(Date, "now").mockReturnValue(280);
		const node = (
			<ServiceNode
				service={buildingService()}
				pos={{ x: 128, y: 128 }}
				selected={false}
				onMouseDown={() => {}}
				onSelect={() => {}}
			/>
		);
		const html = renderToString(node);
		now.mockReturnValue(965);

		const errors: string[] = [];
		const spy = vi.spyOn(console, "error").mockImplementation((...args) => {
			errors.push(args.map(String).join(" "));
		});

		const container = document.createElement("div");
		container.innerHTML = html;
		document.body.appendChild(container);
		await act(async () => {
			hydrateRoot(container, node);
		});

		expect(errors.join("\n")).not.toMatch(/hydrat/i);
		spy.mockRestore();
		now.mockRestore();
		container.remove();
	});
});
