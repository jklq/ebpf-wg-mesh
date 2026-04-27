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
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { DashboardPage } from "./dashboard-page";

const {
	doRedeployServiceMock,
	doCreateServiceFastMock,
	doSaveServicePositionMock,
	fetchGitHubCatalogMock,
	routerMock,
} = vi.hoisted(() => ({
	doCreateServiceFastMock: vi.fn(),
	doRedeployServiceMock: vi.fn(),
	doSaveServicePositionMock: vi.fn(),
	fetchGitHubCatalogMock: vi.fn(),
	routerMock: { invalidate: vi.fn() },
}));

vi.mock("@tanstack/react-router", async (importOriginal) => ({
	...(await importOriginal<typeof import("@tanstack/react-router")>()),
	useRouter: () => routerMock,
}));

vi.mock("./server-fns", () => ({
	doCreateServiceFast: doCreateServiceFastMock,
	doDiscardServiceChanges: vi.fn(),
	doRedeployService: doRedeployServiceMock,
	doSaveServicePosition: doSaveServicePositionMock,
	fetchGitHubCatalog: fetchGitHubCatalogMock,
}));

class MockEventSource {
	static instances: MockEventSource[] = [];

	readonly url: string;
	readonly close = vi.fn();
	readonly listeners = new Map<
		string,
		Array<(event: MessageEvent<string>) => void>
	>();
	readonly addEventListener = vi.fn(
		(type: string, listener: (event: MessageEvent<string>) => void) => {
			this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
		},
	);
	onerror: (() => void) | null = null;

	constructor(url: string) {
		this.url = url;
		MockEventSource.instances.push(this);
	}

	emit(type: string, data: unknown) {
		for (const listener of this.listeners.get(type) ?? []) {
			listener(new MessageEvent(type, { data: JSON.stringify(data) }));
		}
	}
}

beforeEach(() => {
	MockEventSource.instances = [];
	doRedeployServiceMock.mockReset();
	doCreateServiceFastMock.mockReset();
	doSaveServicePositionMock.mockReset();
	fetchGitHubCatalogMock.mockReset();
	fetchGitHubCatalogMock.mockResolvedValue({
		githubAccount: undefined,
		repositories: [],
	});
	routerMock.invalidate.mockReset();
	Object.defineProperty(HTMLElement.prototype, "clientWidth", {
		configurable: true,
		value: 1000,
	});
	Object.defineProperty(HTMLElement.prototype, "clientHeight", {
		configurable: true,
		value: 800,
	});
	Object.defineProperty(window, "innerWidth", {
		configurable: true,
		value: 1000,
	});
	Object.defineProperty(window, "innerHeight", {
		configurable: true,
		value: 848,
	});
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

describe("DashboardPage", () => {
	it("keeps the service status event stream open when the project object refreshes with the same id", async () => {
		const service = serviceRecord();
		const { rerender } = render(
			<DashboardPage state={dashboardState(service)} />,
		);

		await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
		expect(MockEventSource.instances[0]?.url).toBe(
			"/events/project-services?projectId=project-1",
		);

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));

		await waitFor(() => expect(MockEventSource.instances).toHaveLength(2));
		const statusSource = MockEventSource.instances.find((source) =>
			source.url.includes("/events/service-status"),
		);
		expect(statusSource?.url).toBe(
			"/events/service-status?projectId=project-1&serviceId=service-1",
		);

		rerender(
			<DashboardPage
				state={dashboardState(service, {
					project: { id: "project-1", name: "renamed", kind: "user" },
				})}
			/>,
		);

		await waitFor(() =>
			expect(screen.getAllByText("renamed").length).toBeGreaterThan(0),
		);
		expect(MockEventSource.instances).toHaveLength(2);
		expect(statusSource?.close).not.toHaveBeenCalled();
	});

	it("uses service layout positions from the initial state", async () => {
		render(
			<DashboardPage
				state={dashboardState(
					serviceRecord({ layoutPosition: { x: 320, y: 256 } }),
				)}
			/>,
		);

		const node = screen.getByRole("button", { name: /hello/i });
		expect(node.style.left).toBe("320px");
		expect(node.style.top).toBe("256px");
	});

	it("renders service nodes without initial service status", () => {
		render(
			<DashboardPage
				state={dashboardState(serviceRecord(), { serviceStatus: undefined })}
			/>,
		);

		expect(screen.getByRole("button", { name: /hello/i })).toBeTruthy();
	});

	it("updates unselected service badges from the project service stream", async () => {
		const service = serviceRecord();
		const worker = serviceRecord({
			id: "service-2",
			name: "worker",
			specRevision: 2,
			pendingChanges: false,
			unappliedChangeCount: 0,
			unappliedChanges: [],
		});
		render(
			<DashboardPage
				state={dashboardState(service, {
					services: [service, worker],
				})}
			/>,
		);

		const projectSource = await waitFor(() => {
			const source = MockEventSource.instances.find((entry) =>
				entry.url.includes("/events/project-services"),
			);
			expect(source).toBeTruthy();
			return source;
		});
		projectSource?.emit("services", [
			service,
			{
				...worker,
				pendingChanges: true,
				unappliedChangeCount: 2,
				unappliedChanges: [
					unappliedChange("runtime.env.FOO", "Variables", "FOO", "", "one"),
					unappliedChange("runtime.env.BAR", "Variables", "BAR", "", "two"),
				],
			},
		]);

		expect(await screen.findByText("2 changes")).toBeTruthy();
	});

	it("loads the GitHub catalog when New Service opens", async () => {
		render(<DashboardPage state={dashboardState(serviceRecord())} />);

		fireEvent.click(screen.getByRole("button", { name: /deploy service/i }));

		await waitFor(() =>
			expect(fetchGitHubCatalogMock).toHaveBeenCalledTimes(1),
		);
		expect(
			await screen.findByPlaceholderText("Search repositories…"),
		).toBeTruthy();
	});

	it("loads the GitHub catalog when Settings opens", async () => {
		render(<DashboardPage state={dashboardState(serviceRecord())} />);

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));
		fireEvent.click(await screen.findByRole("button", { name: /settings/i }));

		await waitFor(() =>
			expect(fetchGitHubCatalogMock).toHaveBeenCalledTimes(1),
		);
	});

	it("centers the initial viewport around services", () => {
		const { container } = render(
			<DashboardPage state={dashboardState(serviceRecord())} />,
		);

		const world = container.querySelector(".canvas-world") as HTMLElement;

		expect(world.style.transform).toBe("translate(256px,256px) scale(1)");
	});

	it("pans the canvas with trackpad wheel gestures over a service", async () => {
		const { container } = render(
			<DashboardPage state={dashboardState(serviceRecord())} />,
		);

		const node = screen.getByRole("button", { name: /hello/i });
		const world = container.querySelector(".canvas-world") as HTMLElement;
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

		const canvas = container.querySelector(".canvas-grid") as HTMLElement;
		const node = screen.getByRole("button", { name: /hello/i });
		const world = container.querySelector(".canvas-world") as HTMLElement;
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

	it("reveals the canvas when it receives size after a service is selected", async () => {
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
		const world = container.querySelector(".canvas-world") as HTMLElement;
		expect(world.style.visibility).toBe("hidden");

		fireEvent.click(container.querySelector(".service-node") as HTMLElement);
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
				projectId: "project-1",
				serviceId: "service-1",
				position: { x: 192, y: 160 },
			},
		});
	});

	it("does not poll the whole route while a service is building", () => {
		const setIntervalSpy = vi.spyOn(window, "setInterval");

		render(
			<DashboardPage
				state={dashboardState(
					serviceRecord({
						latestBuild: {
							buildId: "build-1",
							state: "running",
							commitSha: "abc123",
							imageDigest: "",
							queuedAt: new Date(),
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
					}),
				)}
			/>,
		);

		expect(setIntervalSpy).not.toHaveBeenCalled();
		expect(routerMock.invalidate).not.toHaveBeenCalled();
	});

	it("marks the active deploy as applying and lets a newer deploy queue", async () => {
		const firstDeploy = deferred<{
			service: DashboardServiceRecord;
		}>();
		const secondDeploy = deferred<{
			service: DashboardServiceRecord;
		}>();
		doRedeployServiceMock
			.mockReturnValueOnce(firstDeploy.promise)
			.mockReturnValueOnce(secondDeploy.promise);

		const firstEdit = serviceRecord({
			specRevision: 2,
			pendingChanges: true,
			unappliedChangeCount: 1,
			unappliedChanges: [
				unappliedChange("runtime.env.FOO", "Variables", "FOO", "old", "new"),
			],
		});
		const { rerender } = render(
			<DashboardPage state={dashboardState(firstEdit)} />,
		);

		fireEvent.click(screen.getByRole("button", { name: "Deploy" }));

		await screen.findByText("Applying 1 change");
		expect(doRedeployServiceMock).toHaveBeenCalledTimes(1);

		const secondEdit = serviceRecord({
			specRevision: 3,
			pendingChanges: true,
			unappliedChangeCount: 2,
			unappliedChanges: [
				unappliedChange("runtime.env.FOO", "Variables", "FOO", "old", "new"),
				unappliedChange("runtime.env.BAR", "Variables", "BAR", "", "next"),
			],
		});
		rerender(<DashboardPage state={dashboardState(secondEdit)} />);

		await screen.findByText("Applying 1 change, 1 ready");
		fireEvent.click(screen.getByRole("button", { name: "Queue deploy" }));
		await screen.findByText("Applying 1 change, next deploy queued");

		firstDeploy.resolve({
			service: serviceRecord({
				specRevision: 2,
				pendingChanges: false,
				unappliedChangeCount: 0,
				unappliedChanges: [],
			}),
		});

		await waitFor(() => expect(doRedeployServiceMock).toHaveBeenCalledTimes(2));
		expect(doRedeployServiceMock).toHaveBeenNthCalledWith(2, {
			data: { projectId: "project-1", serviceId: "service-1" },
		});

		secondDeploy.resolve({
			service: serviceRecord({
				specRevision: 3,
				pendingChanges: false,
				unappliedChangeCount: 0,
				unappliedChanges: [],
			}),
		});
		await waitFor(() => expect(routerMock.invalidate).toHaveBeenCalled());
	});
});

function dashboardState(
	service: DashboardServiceRecord,
	overrides: Partial<DashboardHomeState> = {},
): DashboardHomeState {
	return {
		user: {
			id: "user-1",
			subject: "user-1",
			email: "user@example.com",
		},
		project: { id: "project-1", name: "test-project", kind: "user" },
		onboarding: {
			currentStep: "build",
			projectId: "project-1",
			serviceId: service.id,
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		services: [service],
		service,
		serviceStatus: undefined,
		githubInstallURL:
			"https://github.example.test/apps/platform/installations/new",
		publicBaseURL: "https://dashboard.example.test",
		localIngressBaseURL: undefined,
		ingressTargetHost: "platform.example.test",
		localDomainSuffix: undefined,
		domainBindings: [],
		controlPlaneReachable: true,
		...overrides,
	};
}

function serviceRecord(
	overrides: Partial<DashboardServiceRecord> = {},
): DashboardServiceRecord {
	return {
		id: "service-1",
		projectId: "project-1",
		name: "hello",
		spec: {
			source: {
				provider: "github",
				repositorySelector: "octocat/hello",
				trackedRef: "main",
			},
			runtime: { env: {}, ports: [] },
		},
		...overrides,
	};
}

function unappliedChange(
	id: string,
	section: string,
	field: string,
	currentValue: string,
	newValue: string,
): NonNullable<DashboardServiceRecord["unappliedChanges"]>[number] {
	return {
		id,
		section,
		field,
		path: id,
		action: currentValue ? "update" : "add",
		currentValue,
		newValue,
	};
}

function deferred<T>() {
	let resolve!: (value: T) => void;
	let reject!: (error: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}
