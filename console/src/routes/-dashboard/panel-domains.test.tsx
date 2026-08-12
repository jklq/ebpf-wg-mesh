// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { PanelDomains } from "./panel-domains";

const serverFns = vi.hoisted(() => ({
	create: vi.fn(),
	generate: vi.fn(),
	list: vi.fn(),
}));

vi.mock("./server-fns", () => ({
	doCreateDomainBinding: serverFns.create,
	doDeleteDomainBinding: vi.fn(),
	doGenerateDomainBinding: serverFns.generate,
	doUpdateDomainBinding: vi.fn(),
	fetchDomainBindings: serverFns.list,
}));

describe("domains panel", () => {
	afterEach(cleanup);

	beforeEach(() => {
		serverFns.create.mockReset();
		serverFns.generate.mockReset();
		serverFns.list.mockReset().mockResolvedValue([]);
	});

	it("offers separate generated and custom domain flows and keeps CNAME instructions pending", async () => {
		const platformBinding: DashboardDomainBinding = {
			hostname: "violet-7k3.platform.example",
			projectId: "project-1",
			serviceId: "service-1",
			targetPort: 8080,
			platformGenerated: true,
		};
		serverFns.generate.mockResolvedValue(platformBinding);
		serverFns.create.mockRejectedValue(new Error("CNAME has not verified yet"));

		render(
			<PanelDomains
				service={
					{
						id: "service-1",
						environmentId: "environment-1",
						projectId: "project-1",
						name: "web",
						internalHostname: "accurate-reflection.mesh.internal",
						spec: {
							runtime: {
								env: {},
								cpuMillis: 250,
								memoryMebibytes: 256,
								ports: [],
							},
						},
					} as DashboardServiceRecord
				}
				state={{ domainBindings: [] } as unknown as DashboardHomeState}
			/>,
		);

		expect(
			await screen.findByRole("button", { name: "Generate Domain" }),
		).toBeTruthy();
		expect(screen.getByText("accurate-reflection.mesh.internal")).toBeTruthy();
		expect(screen.getByText("accurate-reflection", { selector: "code" })).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: "Custom Domain" }));
		expect(screen.getByRole("dialog", { name: "Custom domain" })).toBeTruthy();
		expect(
			screen.getByRole("button", { name: "Generate & continue" }),
		).toBeTruthy();

		fireEvent.click(
			screen.getByRole("button", { name: "Generate & continue" }),
		);
		fireEvent.change(await screen.findByLabelText("Custom hostname"), {
			target: { value: "app.customer.com" },
		});

		expect(
			screen.getByText(
				/app\.customer\.com CNAME violet-7k3\.platform\.example/,
			),
		).toBeTruthy();
		fireEvent.click(
			screen.getByRole("button", { name: "Verify CNAME & add domain" }),
		);

		await waitFor(() =>
			expect(screen.getByText("CNAME has not verified yet")).toBeTruthy(),
		);
		expect(
			screen.getByText(
				/app\.customer\.com CNAME violet-7k3\.platform\.example/,
			),
		).toBeTruthy();
	});
});
