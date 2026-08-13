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

import { DashboardPage, resetDashboardPageTestState } from "./dashboard-page";

const {
	doDeployEnvironmentMock,
	doCreateServiceFastMock,
	doSaveServicePositionMock,
	fetchGitHubCatalogMock,
	routerMock,
} = vi.hoisted(() => ({
	doCreateServiceFastMock: vi.fn(),
	doDeployEnvironmentMock: vi.fn(),
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
	doDeployEnvironment: doDeployEnvironmentMock,
	doDiscardServiceChanges: vi.fn(),
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
	doDeployEnvironmentMock.mockReset();
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
	resetDashboardPageTestState();
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
			"/events/environment-services?environmentId=environment-1",
		);

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));

		await waitFor(() => expect(MockEventSource.instances).toHaveLength(2));
		const statusSource = MockEventSource.instances.find((source) =>
			source.url.includes("/events/service-status"),
		);
		expect(statusSource?.url).toBe(
			"/events/service-status?serviceId=service-1",
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

	it("updates unselected service badges from the environment service stream", async () => {
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

		const environmentSource = await waitFor(() => {
			const source = MockEventSource.instances.find((entry) =>
				entry.url.includes("/events/environment-services"),
			);
			expect(source).toBeTruthy();
			return source;
		});
		environmentSource?.emit("services", [
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

	it("replaces subscriptions and ignores stale events when the environment changes", async () => {
		const firstService = serviceRecord();
		const { rerender } = render(
			<DashboardPage state={dashboardState(firstService)} />,
		);
		const firstSource = await waitFor(() => {
			const source = MockEventSource.instances.find((entry) =>
				entry.url.includes("environmentId=environment-1"),
			);
			expect(source).toBeTruthy();
			return source as MockEventSource;
		});

		const secondService = serviceRecord({
			id: "service-2",
			environmentId: "environment-2",
			name: "worker",
		});
		const secondEnvironment = {
			id: "environment-2",
			projectId: "project-1",
			name: "Staging",
			kind: "persistent" as const,
			isProduction: false,
		};
		rerender(
			<DashboardPage
				state={dashboardState(secondService, {
					environments: [
						{
							id: "environment-1",
							projectId: "project-1",
							name: "Production",
							kind: "persistent",
							isProduction: true,
						},
						secondEnvironment,
					],
					environment: secondEnvironment,
					services: [secondService],
					service: secondService,
				})}
			/>,
		);

		await screen.findByRole("button", { name: /worker/i });
		await waitFor(() => expect(firstSource.close).toHaveBeenCalledTimes(1));
		expect(
			MockEventSource.instances.some((entry) =>
				entry.url.includes("environmentId=environment-2"),
			),
		).toBe(true);

		firstSource.emit("services", [firstService]);
		await Promise.resolve();
		expect(screen.queryByRole("button", { name: /^hello\b/i })).toBeNull();
		expect(screen.getByRole("button", { name: /worker/i })).toBeTruthy();
	});

	it("loads the GitHub catalog from repository controls", async () => {
		render(<DashboardPage state={dashboardState(serviceRecord())} />);

		fireEvent.click(screen.getByRole("button", { name: /deploy service/i }));

		await waitFor(() =>
			expect(fetchGitHubCatalogMock).toHaveBeenCalledTimes(1),
		);
		expect(
			await screen.findByPlaceholderText("Search repositories…"),
		).toBeTruthy();

		cleanup();
		render(<DashboardPage state={dashboardState(serviceRecord())} />);

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));
		fireEvent.click(await screen.findByRole("button", { name: /settings/i }));

		await waitFor(() =>
			expect(fetchGitHubCatalogMock).toHaveBeenCalledTimes(2),
		);
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
				environmentId: "environment-1",
				serviceId: "service-1",
				position: { x: 192, y: 160 },
			},
		});
	});

	it("marks the active deploy as applying and lets a newer deploy queue", async () => {
		const firstDeploy = deferred<Array<{ service: DashboardServiceRecord }>>();
		const secondDeploy = deferred<Array<{ service: DashboardServiceRecord }>>();
		doDeployEnvironmentMock
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
		expect(doDeployEnvironmentMock).toHaveBeenCalledTimes(1);

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

		firstDeploy.resolve([
			{
				service: serviceRecord({
					specRevision: 2,
					pendingChanges: false,
					unappliedChangeCount: 0,
					unappliedChanges: [],
				}),
			},
		]);

		await waitFor(() =>
			expect(doDeployEnvironmentMock).toHaveBeenCalledTimes(2),
		);
		expect(doDeployEnvironmentMock).toHaveBeenNthCalledWith(2, {
			data: { environmentId: "environment-1" },
		});

		secondDeploy.resolve([
			{
				service: serviceRecord({
					specRevision: 3,
					pendingChanges: false,
					unappliedChangeCount: 0,
					unappliedChanges: [],
				}),
			},
		]);
		await waitFor(() => expect(routerMock.invalidate).toHaveBeenCalled());
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

		const { rerender } = render(<DashboardPage state={emptyState()} />);

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

		// The server snapshot still predates the created service.
		rerender(<DashboardPage state={emptyState()} />);

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

		const { unmount } = render(<DashboardPage state={emptyState()} />);

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

		unmount();

		// `/` redirects to `/environments/:id` after invalidate, remounting
		// DashboardPage. Selection must survive that remount.
		render(<DashboardPage state={emptyState()} />);

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
			/>,
		);

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();
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

		const { unmount } = render(<DashboardPage state={emptyState()} />);

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
		environmentSource.emit("services", [created]);

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();

		unmount();
		render(
			<DashboardPage
				state={dashboardState(created, {
					environment,
					environments: [environment],
				})}
			/>,
		);

		expect(
			screen.getByRole("button", { name: /close service panel/i }),
		).toBeTruthy();
	});
});

function dashboardState(
	service: DashboardServiceRecord,
	overrides: Partial<DashboardHomeState> = {},
): DashboardHomeState {
	return {
		user: {
			id: "user-1",
			email: "user@example.com",
		},
		project: { id: "project-1", name: "test-project", kind: "user" },
		environments: [
			{
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "persistent",
				isProduction: true,
			},
		],
		environment: {
			id: "environment-1",
			projectId: "project-1",
			name: "Production",
			kind: "persistent",
			isProduction: true,
		},
		onboarding: {
			currentStep: "build",
			projectId: "project-1",
			environmentId: "environment-1",
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

function emptyState(): DashboardHomeState {
	return dashboardState(serviceRecord(), {
		services: [],
		service: undefined,
		environment: undefined,
		githubAccount: {
			providerSubject: "1",
			login: "octocat",
			primaryEmail: "octocat@example.com",
			tokenType: "bearer",
			scope: "repo",
		},
		repositories: [
			{
				owner: "octocat",
				name: "hello",
				fullName: "octocat/hello",
				private: false,
				defaultBranch: "main",
			},
		],
	});
}

function serviceRecord(
	overrides: Partial<DashboardServiceRecord> = {},
): DashboardServiceRecord {
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
