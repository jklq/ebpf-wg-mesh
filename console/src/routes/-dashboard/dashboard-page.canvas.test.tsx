// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CreatedServiceCacheProvider } from "./created-service-cache";
import { DashboardPage } from "./dashboard-page";
import {
	dashboardState,
	deferred,
	emptyState,
	MockEventSource,
	serviceRecord,
	stubDashboardLayoutMetrics,
} from "./dashboard-page.test-helpers";

const {
	doReleaseEnvironmentMock,
	doCreateServiceFastMock,
	doSaveServicePositionMock,
	doUpdateServiceMock,
	fetchGitHubCatalogMock,
	routerMock,
} = vi.hoisted(() => ({
	doCreateServiceFastMock: vi.fn(),
	doReleaseEnvironmentMock: vi.fn(),
	doSaveServicePositionMock: vi.fn(),
	doUpdateServiceMock: vi.fn(),
	fetchGitHubCatalogMock: vi.fn(),
	routerMock: { invalidate: vi.fn(), navigate: vi.fn() },
}));

vi.mock("@tanstack/react-router", async (importOriginal) => ({
	...(await importOriginal<typeof import("@tanstack/react-router")>()),
	useRouter: () => routerMock,
}));

vi.mock("./server-fns", () => ({
	doCreateServiceFast: doCreateServiceFastMock,
	doReleaseEnvironment: doReleaseEnvironmentMock,
	doDiscardServiceChanges: vi.fn(),
	doSaveServicePosition: doSaveServicePositionMock,
	doUpdateService: doUpdateServiceMock,
	fetchGitHubCatalog: fetchGitHubCatalogMock,
}));

beforeEach(() => {
	MockEventSource.instances = [];
	doReleaseEnvironmentMock.mockReset();
	doCreateServiceFastMock.mockReset();
	doSaveServicePositionMock.mockReset();
	doUpdateServiceMock.mockReset();
	fetchGitHubCatalogMock.mockReset();
	fetchGitHubCatalogMock.mockResolvedValue({
		githubAccount: undefined,
		repositories: [],
	});
	routerMock.invalidate.mockReset();
	routerMock.navigate.mockReset();
	stubDashboardLayoutMetrics();
	vi.stubGlobal("EventSource", MockEventSource);
	vi.stubGlobal(
		"requestAnimationFrame",
		vi.fn(() => 1),
	);
	vi.stubGlobal("cancelAnimationFrame", vi.fn());
});

afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
});

describe("DashboardPage canvas", () => {
	it("pans the canvas with trackpad wheel gestures over a service", async () => {
		const { container } = render(
			<DashboardPage state={dashboardState(serviceRecord())} />,
		);

		const node = screen.getByRole("button", { name: /hello/i });
		const world = container.querySelector("[data-canvas-world]") as HTMLElement;
		node.dispatchEvent(
			new WheelEvent("wheel", {
				bubbles: true,
				cancelable: true,
				deltaX: 40,
				deltaY: 20,
			}),
		);

		await waitFor(() =>
			expect(world.style.transform).toBe("translate(216px,236px) scale(1)"),
		);
	});

	it("zooms the canvas around the trackpad pinch point over a service", async () => {
		const { container } = render(
			<DashboardPage state={dashboardState(serviceRecord())} />,
		);

		const canvas = screen.getByRole("application");
		const node = screen.getByRole("button", { name: /hello/i });
		const world = container.querySelector("[data-canvas-world]") as HTMLElement;
		canvas.getBoundingClientRect = () =>
			({
				left: 0,
				top: 0,
				right: 1000,
				bottom: 800,
				width: 1000,
				height: 800,
				x: 0,
				y: 0,
				toJSON: () => ({}),
			}) as DOMRect;
		node.dispatchEvent(
			new WheelEvent("wheel", {
				bubbles: true,
				cancelable: true,
				ctrlKey: true,
				clientX: 500,
				clientY: 400,
				deltaY: -100,
			}),
		);

		const nextZoom = Math.exp(0.6);
		await waitFor(() =>
			expect(world.style.transform).toBe(
				`translate(${500 - (500 - 256) * nextZoom}px,${400 - (400 - 256) * nextZoom}px) scale(${nextZoom})`,
			),
		);
	});

	it("closes the selected service panel with Escape", async () => {
		render(<DashboardPage state={dashboardState(serviceRecord())} />);

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));
		expect(
			await screen.findByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		fireEvent.keyDown(document, { key: "Escape" });

		await waitFor(() =>
			expect(
				screen.queryByRole("button", { name: /close service panel/i }),
			).toBeNull(),
		);
	});

	it("centers the undeployed banner inside the visible canvas", async () => {
		const dirtyService = serviceRecord({
			pendingChanges: true,
			unappliedChangeCount: 1,
		});
		const { container } = render(
			<DashboardPage state={dashboardState(dirtyService)} />,
		);
		const prompt = container.querySelector(
			"[data-workspace-prompt]",
		) as HTMLElement;
		expect(prompt.style.right).toBe("0px");

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));

		await waitFor(() =>
			expect(prompt.style.right).toBe("var(--spacing-side-panel)"),
		);
	});

	it("reveals and centers the canvas when it receives a size", async () => {
		let width = 0;
		let height = 0;
		Object.defineProperty(HTMLElement.prototype, "clientWidth", {
			configurable: true,
			get: () => width,
		});
		Object.defineProperty(HTMLElement.prototype, "clientHeight", {
			configurable: true,
			get: () => height,
		});

		const { container } = render(
			<DashboardPage state={dashboardState(serviceRecord())} />,
		);
		const world = container.querySelector("[data-canvas-world]") as HTMLElement;
		expect(world.style.visibility).toBe("hidden");

		fireEvent.click(
			container.querySelector("[data-service-node]") as HTMLElement,
		);
		width = 1000;
		height = 800;
		fireEvent(window, new Event("resize"));

		await waitFor(() => {
			expect(world.style.visibility).toBe("visible");
			expect(world.style.transform).toBe("translate(256px,256px) scale(1)");
		});
	});

	it("saves service positions after dragging", async () => {
		render(<DashboardPage state={dashboardState(serviceRecord())} />);

		const node = screen.getByRole("button", { name: /hello/i });
		fireEvent.mouseDown(node, { button: 0, clientX: 0, clientY: 0 });
		fireEvent.mouseMove(window, { clientX: 70, clientY: 35 });
		fireEvent.mouseUp(window);

		expect(doSaveServicePositionMock).toHaveBeenCalledWith({
			data: {
				environmentId: "environment-1",
				serviceId: "service-1",
				position: { x: 192, y: 160 },
			},
		});
	});

	it("keeps the panel open for a new service while the server state catches up", async () => {
		const created = serviceRecord();
		doCreateServiceFastMock.mockResolvedValue({
			project: { id: "project-1", name: "test-project", kind: "user" },
			environment: {
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "persistent",
				isProduction: true,
			},
			service: created,
			serviceStatus: null,
			onboarding: emptyState().onboarding,
		});

		const { rerender } = render(
			<CreatedServiceCacheProvider>
				<DashboardPage state={emptyState()} />
			</CreatedServiceCacheProvider>,
		);

		fireEvent.click(
			screen.getAllByRole("button", {
				name: /deploy service/i,
			})[0] as HTMLElement,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: /octocat\/hello/i }),
		);

		expect(
			await screen.findByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		rerender(
			<CreatedServiceCacheProvider>
				<DashboardPage state={emptyState()} />
			</CreatedServiceCacheProvider>,
		);

		expect(routerMock.invalidate).not.toHaveBeenCalled();
		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();
	});

	it("keeps the panel open after remounting when the first service creates an environment", async () => {
		const created = serviceRecord();
		const environment = {
			id: "environment-1",
			projectId: "project-1",
			name: "Production",
			kind: "persistent" as const,
			isProduction: true,
		};
		doCreateServiceFastMock.mockResolvedValue({
			project: { id: "project-1", name: "test-project", kind: "user" },
			environment,
			service: created,
			serviceStatus: null,
			onboarding: emptyState().onboarding,
		});

		const { rerender } = render(
			<CreatedServiceCacheProvider>
				<DashboardPage key="before-remount" state={emptyState()} />
			</CreatedServiceCacheProvider>,
		);

		fireEvent.click(
			screen.getAllByRole("button", {
				name: /deploy service/i,
			})[0] as HTMLElement,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: /octocat\/hello/i }),
		);

		expect(
			await screen.findByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		rerender(
			<CreatedServiceCacheProvider>
				<DashboardPage
					key="after-remount"
					state={emptyState()}
					urlSelectedServiceId={created.id}
				/>
			</CreatedServiceCacheProvider>,
		);

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		cleanup();
		render(
			<DashboardPage
				state={dashboardState(created, {
					environment,
					environments: [environment],
				})}
				urlSelectedServiceId={created.id}
			/>,
		);

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();
	});

	it("renders a selectable loading service on the canvas immediately", async () => {
		const creation = deferred<{
			project: { id: string; name: string; kind: string };
			environment: {
				id: string;
				projectId: string;
				name: string;
				kind: "persistent";
				isProduction: boolean;
			};
			service: ReturnType<typeof serviceRecord>;
			serviceStatus: null;
			onboarding: ReturnType<typeof emptyState>["onboarding"];
		}>();
		doCreateServiceFastMock.mockReturnValue(creation.promise);

		const { container } = render(<DashboardPage state={emptyState()} />);

		fireEvent.click(
			screen.getAllByRole("button", {
				name: /deploy service/i,
			})[0] as HTMLElement,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: /octocat\/hello/i }),
		);

		await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
		const pendingNode = container.querySelector(
			"[data-service-node]",
		) as HTMLButtonElement;
		expect(pendingNode).toBeTruthy();
		expect(pendingNode.textContent).toMatch(/creating service/i);
		expect(screen.getByText("Creating service")).toBeTruthy();

		fireEvent.click(
			screen.getByRole("button", { name: /close service panel/i }),
		);
		await waitFor(() =>
			expect(
				screen.queryByRole("button", { name: /close service panel/i }),
			).toBeNull(),
		);
		fireEvent.click(pendingNode);
		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		const created = serviceRecord();
		creation.resolve({
			project: { id: "project-1", name: "test-project", kind: "user" },
			environment: {
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "persistent",
				isProduction: true,
			},
			service: created,
			serviceStatus: null,
			onboarding: emptyState().onboarding,
		});

		expect(
			await screen.findByRole("button", { name: /close service panel/i }),
		).toBeTruthy();
		const createdNode = container.querySelector("[data-service-node]");
		expect(createdNode).toBeTruthy();
		expect(createdNode?.textContent).toMatch(/hello/i);
		expect(createdNode?.textContent).not.toMatch(/creating service/i);
	});

	it("reopens the repository picker with the error when creation fails", async () => {
		doCreateServiceFastMock.mockRejectedValue(
			new Error("Repository access is not available yet."),
		);

		const { container } = render(<DashboardPage state={emptyState()} />);

		fireEvent.click(
			screen.getAllByRole("button", {
				name: /deploy service/i,
			})[0] as HTMLElement,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: /octocat\/hello/i }),
		);

		expect(
			await screen.findByText(/Repository access is not available yet\./),
		).toBeTruthy();
		expect(container.querySelectorAll("[data-service-node]")).toHaveLength(0);
	});

	it("keeps the panel open after a live snapshot and remount for the first service", async () => {
		const created = serviceRecord();
		const environment = {
			id: "environment-1",
			projectId: "project-1",
			name: "Production",
			kind: "persistent" as const,
			isProduction: true,
		};
		doCreateServiceFastMock.mockResolvedValue({
			project: { id: "project-1", name: "test-project", kind: "user" },
			environment,
			service: created,
			serviceStatus: null,
			onboarding: emptyState().onboarding,
		});

		const { rerender } = render(
			<CreatedServiceCacheProvider>
				<DashboardPage key="before-remount" state={emptyState()} />
			</CreatedServiceCacheProvider>,
		);

		fireEvent.click(
			screen.getAllByRole("button", {
				name: /deploy service/i,
			})[0] as HTMLElement,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: /octocat\/hello/i }),
		);
		expect(
			await screen.findByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		const environmentSource = await waitFor(() => {
			const source = MockEventSource.instances.find((entry) =>
				entry.url.includes("environmentId=environment-1"),
			);
			expect(source).toBeTruthy();
			return source as MockEventSource;
		});
		environmentSource.emit("services", {
			services: [created],
			revision: 2,
		});

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		rerender(
			<CreatedServiceCacheProvider>
				<DashboardPage
					key="after-remount"
					state={emptyState()}
					urlSelectedServiceId={created.id}
				/>
			</CreatedServiceCacheProvider>,
		);

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		cleanup();
		render(
			<DashboardPage
				state={dashboardState(created, {
					environment,
					environments: [environment],
				})}
				urlSelectedServiceId={created.id}
			/>,
		);

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();
	});
});
