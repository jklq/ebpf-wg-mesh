// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
	within,
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

	it("closes the generate dialog immediately and shows the domain as pending", async () => {
		const platformBinding: DashboardDomainBinding = {
			hostname: "violet-7k3.platform.example",
			projectId: "project-1",
			serviceId: "service-1",
			targetPort: 8080,
			platformGenerated: true,
			ownershipState: "DOMAIN_OWNERSHIP_STATE_VERIFIED",
		};
		let resolveGenerate: (binding: DashboardDomainBinding) => void = () => {};
		serverFns.generate.mockReturnValue(
			new Promise<DashboardDomainBinding>((resolve) => {
				resolveGenerate = resolve;
			}),
		);

		render(<PanelDomains service={domainService()} state={domainState()} />);

		fireEvent.click(
			await screen.findByRole("button", { name: "Generate Domain" }),
		);
		const dialog = screen.getByRole("dialog", { name: "Generate domain" });
		const portInput = within(dialog).getByLabelText("App port");
		expect(document.activeElement).toBe(portInput);
		fireEvent.submit(dialog.querySelector("form") as HTMLFormElement);

		await waitFor(() =>
			expect(
				screen.queryByRole("dialog", { name: "Generate domain" }),
			).toBeNull(),
		);
		expect(screen.getByText("Generating domain…")).toBeTruthy();

		resolveGenerate(platformBinding);

		expect(
			await screen.findByText(/violet-7k3\.platform\.example/),
		).toBeTruthy();
		await waitFor(() =>
			expect(screen.queryByText("Generating domain…")).toBeNull(),
		);
	});

	it("renders domain dialogs at the viewport level and consumes Escape", async () => {
		render(<PanelDomains service={domainService()} state={domainState()} />);

		fireEvent.click(
			await screen.findByRole("button", { name: "Generate Domain" }),
		);
		const dialog = screen.getByRole("dialog", { name: "Generate domain" });

		expect(dialog.parentElement).toBe(document.body);
		fireEvent.keyDown(dialog, { key: "Escape" });

		expect(
			screen.queryByRole("dialog", { name: "Generate domain" }),
		).toBeNull();
	});

	it("adds a custom domain without blocking on CNAME and shows ownership status", async () => {
		const platformBinding: DashboardDomainBinding = {
			hostname: "violet-7k3.platform.example",
			projectId: "project-1",
			serviceId: "service-1",
			targetPort: 8080,
			platformGenerated: true,
			ownershipState: "DOMAIN_OWNERSHIP_STATE_VERIFIED",
		};
		const customBinding: DashboardDomainBinding = {
			hostname: "app.customer.com",
			projectId: "project-1",
			serviceId: "service-1",
			targetPort: 8080,
			platformGenerated: false,
			ownershipState: "DOMAIN_OWNERSHIP_STATE_UNVERIFIED",
			ownershipMessage:
				"domain CNAME does not point to the service platform hostname: lookup app.customer.com: no such host",
		};
		serverFns.generate.mockResolvedValue(platformBinding);
		serverFns.create.mockResolvedValue(customBinding);

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
		expect(
			screen.getByText("accurate-reflection", { selector: "code" }),
		).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: "Custom Domain" }));
		const dialog = screen.getByRole("dialog", { name: "Custom domain" });
		expect(dialog).toBeTruthy();
		expect(
			screen.getByRole("button", { name: "Generate & continue" }),
		).toBeTruthy();

		fireEvent.submit(dialog.querySelector("form") as HTMLFormElement);
		fireEvent.change(await screen.findByLabelText("Custom hostname"), {
			target: { value: "app.customer.com" },
		});
		expect(document.activeElement).toBe(
			screen.getByLabelText("Custom hostname"),
		);

		expect(
			screen.getByText(
				/app\.customer\.com CNAME violet-7k3\.platform\.example/,
			),
		).toBeTruthy();
		fireEvent.submit(
			screen
				.getByRole("dialog", { name: "Custom domain" })
				.querySelector("form") as HTMLFormElement,
		);

		await waitFor(() =>
			expect(
				screen.queryByRole("dialog", { name: "Custom domain" }),
			).toBeNull(),
		);
		expect(screen.getByText("Waiting for CNAME")).toBeTruthy();
		expect(
			screen.getByText(
				/DNS does not resolve yet.*violet-7k3\.platform\.example/,
			),
		).toBeTruthy();
		expect(
			screen.getByText(
				/app\.customer\.com CNAME violet-7k3\.platform\.example/,
			),
		).toBeTruthy();
		expect(screen.queryByText(/violet-7k3\.platform\.example\s+->/)).toBeNull();
	});

	it("hides the generated domain once a custom domain exists", async () => {
		const platformBinding: DashboardDomainBinding = {
			hostname: "violet-7k3.platform.example",
			projectId: "project-1",
			serviceId: "service-1",
			targetPort: 8080,
			platformGenerated: true,
			ownershipState: "DOMAIN_OWNERSHIP_STATE_VERIFIED",
		};
		const customBinding: DashboardDomainBinding = {
			hostname: "app.customer.com",
			projectId: "project-1",
			serviceId: "service-1",
			targetPort: 8080,
			platformGenerated: false,
			ownershipState: "DOMAIN_OWNERSHIP_STATE_VERIFIED",
		};
		serverFns.list.mockResolvedValue([platformBinding, customBinding]);

		render(<PanelDomains service={domainService()} state={domainState()} />);

		expect(await screen.findByText(/app\.customer\.com/)).toBeTruthy();
		expect(screen.getByText("Live")).toBeTruthy();
		expect(screen.queryByText(/violet-7k3\.platform\.example/)).toBeNull();
	});
});

function domainService(): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "web",
		internalHostname: "accurate-reflection.mesh.internal",
		spec: {
			runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		},
	} as DashboardServiceRecord;
}

function domainState(): DashboardHomeState {
	return { domainBindings: [] } as unknown as DashboardHomeState;
}
