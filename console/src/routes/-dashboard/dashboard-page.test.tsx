// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import {
	afterEach,
	beforeAll,
	beforeEach,
	describe,
	expect,
	it,
	vi,
} from "vitest";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { DashboardPage } from "./dashboard-page";
import {
	dashboardState,
	deferred,
	MockEventSource,
	serviceRecord,
	stubDashboardLayoutMetrics,
	unappliedChange,
} from "./dashboard-page.test-helpers";
import { resetServicePersistQueueForTests } from "./use-auto-queued-persist";

const {
	doDeleteServiceMock,
	doDiscardServiceChangesMock,
	doReleaseEnvironmentMock,
	doCreateServiceFastMock,
	doSaveServicePositionMock,
	doUpdateServiceMock,
	fetchGitHubCatalogMock,
	routerMock,
} = vi.hoisted(() => ({
	doCreateServiceFastMock: vi.fn(),
	doDeleteServiceMock: vi.fn(),
	doDiscardServiceChangesMock: vi.fn(),
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
	doDeleteService: doDeleteServiceMock,
	doReleaseEnvironment: doReleaseEnvironmentMock,
	doDiscardServiceChanges: doDiscardServiceChangesMock,
	doSaveServicePosition: doSaveServicePositionMock,
	doUpdateService: doUpdateServiceMock,
	fetchGitHubCatalog: fetchGitHubCatalogMock,
}));

beforeAll(async () => {
	await Promise.all([
		import("./service-panel"),
		import("./panel-variables"),
		import("./panel-settings"),
		import("./panel-domains"),
		import("./new-service-modal"),
	]);
});

beforeEach(() => {
	MockEventSource.instances = [];
	doReleaseEnvironmentMock.mockReset();
	doDeleteServiceMock.mockReset();
	doDiscardServiceChangesMock.mockReset();
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
	vi.stubGlobal("requestIdleCallback", (callback: IdleRequestCallback) =>
		window.setTimeout(() => {
			callback({
				didTimeout: false,
				timeRemaining: () => 50,
			} as IdleDeadline);
		}, 0),
	);
	vi.stubGlobal("cancelIdleCallback", (id: number) => {
		window.clearTimeout(id);
	});
});

afterEach(() => {
	cleanup();
	resetServicePersistQueueForTests();
	vi.unstubAllGlobals();
});

describe("DashboardPage", () => {
	it("closes undeployed changes after discarding an existing service's changes", async () => {
		const dirty = serviceRecord({
			rolloutGeneration: 1,
			pendingChanges: true,
			unappliedChangeCount: 1,
			unappliedChanges: [
				unappliedChange("runtime.env.FOO", "Variables", "FOO", "", "bar"),
			],
		});
		doDiscardServiceChangesMock.mockResolvedValue({
			...dirty,
			pendingChanges: false,
			unappliedChangeCount: 0,
			unappliedChanges: [],
		});
		render(<DashboardPage state={dashboardState(dirty)} />);

		fireEvent.click(screen.getByRole("button", { name: "Details" }));
		fireEvent.click(
			screen.getByRole("button", { name: "Discard service changes" }),
		);

		await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
		expect(doDiscardServiceChangesMock).toHaveBeenCalledWith({
			data: { serviceId: "service-1", discardAll: true },
		});
		expect(screen.getByRole("button", { name: /hello/i })).toBeTruthy();
	});

	it("keeps undeployed changes open until the last service is discarded", async () => {
		const first = serviceRecord({
			rolloutGeneration: 1,
			pendingChanges: true,
			unappliedChangeCount: 1,
			unappliedChanges: [
				unappliedChange("runtime.env.FOO", "Variables", "FOO", "", "one"),
			],
		});
		const second = serviceRecord({
			id: "service-2",
			name: "worker",
			rolloutGeneration: 1,
			pendingChanges: true,
			unappliedChangeCount: 1,
			unappliedChanges: [
				unappliedChange("runtime.env.BAR", "Variables", "BAR", "", "two"),
			],
		});
		doDiscardServiceChangesMock.mockImplementation(
			({ data }: { data: { serviceId: string } }) => {
				const service = data.serviceId === first.id ? first : second;
				return Promise.resolve({
					...service,
					pendingChanges: false,
					unappliedChangeCount: 0,
					unappliedChanges: [],
				});
			},
		);
		render(
			<DashboardPage
				state={dashboardState(first, { services: [first, second] })}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Details" }));
		fireEvent.click(
			screen.getAllByRole("button", { name: "Discard service changes" })[0],
		);

		await waitFor(() =>
			expect(
				screen.getAllByRole("button", { name: "Discard service changes" }),
			).toHaveLength(1),
		);
		expect(screen.getByRole("dialog")).toBeTruthy();

		fireEvent.click(
			screen.getByRole("button", { name: "Discard service changes" }),
		);
		await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
	});

	it("removes a never-deployed service when all of its changes are discarded", async () => {
		const staged = serviceRecord({
			rolloutGeneration: 0,
			pendingChanges: true,
			unappliedChangeCount: 1,
			unappliedChanges: [
				unappliedChange("service", "Service", "Service", "", "hello"),
			],
			latestDeployment: {
				deploymentId: "deployment-1",
				state: "DEPLOYMENT_STATE_STAGED",
				causeKind: "DEPLOYMENT_CAUSE_KIND_USER",
				causeId: "user-1",
				reasonCode: "SERVICE_STAGED",
				detail: "Configuration staged",
				specRevision: 1,
				imageDigest: "",
				rolloutGeneration: 0,
			},
		});
		doDeleteServiceMock.mockResolvedValue(undefined);
		render(<DashboardPage state={dashboardState(staged)} />);

		fireEvent.click(screen.getByRole("button", { name: "Details" }));
		fireEvent.click(
			screen.getByRole("button", { name: "Discard service changes" }),
		);

		await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
		expect(doDeleteServiceMock).toHaveBeenCalledWith({
			data: { serviceId: "service-1" },
		});
		expect(doDiscardServiceChangesMock).not.toHaveBeenCalled();
		expect(screen.queryByRole("button", { name: /hello/i })).toBeNull();
	});

	it("keeps Deploy active during a variable write and waits behind it", {
		timeout: 15_000,
	}, async () => {
		const save = deferred<DashboardServiceRecord>();
		doUpdateServiceMock.mockReturnValue(save.promise);
		doReleaseEnvironmentMock.mockResolvedValue([]);
		render(<DashboardPage state={dashboardState(serviceRecord())} />);

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));
		fireEvent.click(await screen.findByRole("button", { name: "Variables" }));
		fireEvent.click(await screen.findByRole("button", { name: "Raw" }));
		fireEvent.change(screen.getByLabelText("Raw variables"), {
			target: { value: "FOO=bar" },
		});
		fireEvent.click(screen.getByRole("button", { name: "Update variables" }));

		const deploy = await screen.findByRole("button", {
			name: "Deploy changes",
		});
		expect((deploy as HTMLButtonElement).disabled).toBe(false);
		fireEvent.click(deploy);
		expect(
			await screen.findByRole("button", { name: "Deploying…" }),
		).toBeTruthy();
		expect(doReleaseEnvironmentMock).not.toHaveBeenCalled();

		save.resolve(
			serviceRecord({
				specRevision: 2,
				pendingChanges: true,
				unappliedChangeCount: 1,
				unappliedChanges: [
					unappliedChange("runtime.env.FOO", "Variables", "FOO", "", "bar"),
				],
			}),
		);

		await waitFor(() =>
			expect(doReleaseEnvironmentMock).toHaveBeenCalledWith({
				data: { environmentId: "environment-1" },
			}),
		);
		expect(doReleaseEnvironmentMock).toHaveBeenCalledTimes(1);
	});

	it("shows saving immediately, then keeps the acknowledged undeployed change", {
		timeout: 15_000,
	}, async () => {
		const save = deferred<DashboardServiceRecord>();
		doUpdateServiceMock.mockReturnValue(save.promise);
		const current = serviceRecord({
			spec: {
				runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
			},
		});
		render(<DashboardPage state={dashboardState(current)} />);

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));
		fireEvent.click(await screen.findByRole("button", { name: "Variables" }));
		fireEvent.click(await screen.findByRole("button", { name: "Raw" }));
		fireEvent.change(screen.getByLabelText("Raw variables"), {
			target: { value: "FOO=bar" },
		});
		expect(doUpdateServiceMock).not.toHaveBeenCalled();

		fireEvent.click(screen.getByRole("button", { name: "Update variables" }));

		expect(await screen.findByText("Updating undeployed changes")).toBeTruthy();
		await waitFor(() => expect(doUpdateServiceMock).toHaveBeenCalledTimes(1));
		expect(doUpdateServiceMock).toHaveBeenCalledWith({
			data: {
				serviceId: "service-1",
				runtimeEnv: { FOO: "bar" },
			},
		});
		save.resolve(
			serviceRecord({
				specRevision: 2,
				spec: {
					runtime: {
						env: { FOO: "bar" },
						cpuMillis: 250,
						memoryMebibytes: 256,
						ports: [],
					},
				},
				pendingChanges: true,
				unappliedChangeCount: 1,
				unappliedChanges: [
					unappliedChange("runtime.env.FOO", "Variables", "FOO", "", "bar"),
				],
			}),
		);

		expect(await screen.findByText("Undeployed changes")).toBeTruthy();
	});

	it("does not let a stale loader rerender clear undeployed changes", async () => {
		const current = serviceRecord({
			specRevision: 2,
			pendingChanges: true,
			unappliedChangeCount: 1,
			unappliedChanges: [
				unappliedChange("runtime.env.FOO", "Variables", "FOO", "", "bar"),
			],
		});
		const { rerender } = render(
			<DashboardPage
				state={dashboardState(current, { servicesRevision: 2 })}
			/>,
		);
		expect(await screen.findByText("Undeployed changes")).toBeTruthy();

		rerender(
			<DashboardPage
				state={dashboardState(
					serviceRecord({
						specRevision: 1,
						pendingChanges: false,
						unappliedChangeCount: 0,
						unappliedChanges: [],
					}),
					{ servicesRevision: 1 },
				)}
			/>,
		);

		expect(await screen.findByText("Undeployed changes")).toBeTruthy();
	});

	it("accepts a newer loader rerender over older stream data", async () => {
		const clean = serviceRecord({
			pendingChanges: false,
			unappliedChangeCount: 0,
			unappliedChanges: [],
		});
		const { rerender } = render(
			<DashboardPage state={dashboardState(clean, { servicesRevision: 1 })} />,
		);

		rerender(
			<DashboardPage
				state={dashboardState(
					serviceRecord({
						specRevision: 2,
						pendingChanges: true,
						unappliedChangeCount: 1,
						unappliedChanges: [
							unappliedChange("runtime.env.FOO", "Variables", "FOO", "", "bar"),
						],
					}),
					{ servicesRevision: 2 },
				)}
			/>,
		);

		expect(await screen.findByText("Undeployed changes")).toBeTruthy();
	});

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
					project: {
						id: "project-1",
						name: "renamed",
						kind: "PROJECT_KIND_USER",
					},
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
		// A delayed duplicate of the loader snapshot must not clear the
		// badge once the newer revision has applied.
		environmentSource?.emit("services", {
			services: [service, worker],
			revision: 1,
		});
		environmentSource?.emit("services", {
			services: [
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
			],
			revision: 2,
		});
		environmentSource?.emit("services", {
			services: [service, worker],
			revision: 1,
		});

		expect(await screen.findByText("2 changes")).toBeTruthy();
		expect(screen.getByRole("button", { name: "Details" })).toBeTruthy();
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
					selectedServiceId: null,
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

	it("keeps deploy single-flight when newer changes arrive", async () => {
		const firstDeploy = deferred<Array<{ service: DashboardServiceRecord }>>();
		const secondDeploy = deferred<Array<{ service: DashboardServiceRecord }>>();
		doReleaseEnvironmentMock
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
			<DashboardPage
				state={dashboardState(firstEdit, { servicesRevision: 2 })}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: "Deploy changes" }));

		await screen.findByText("Applying 1 change");
		expect(doReleaseEnvironmentMock).toHaveBeenCalledTimes(1);

		const secondEdit = serviceRecord({
			specRevision: 3,
			pendingChanges: true,
			unappliedChangeCount: 2,
			unappliedChanges: [
				unappliedChange("runtime.env.FOO", "Variables", "FOO", "old", "new"),
				unappliedChange("runtime.env.BAR", "Variables", "BAR", "", "next"),
			],
		});
		rerender(
			<DashboardPage
				state={dashboardState(secondEdit, { servicesRevision: 3 })}
			/>,
		);

		await screen.findByText("Applying 1 change, 1 ready");
		const deployingButton = screen.getByRole("button", { name: "Deploying…" });
		expect((deployingButton as HTMLButtonElement).disabled).toBe(true);

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

		await waitFor(() => expect(routerMock.invalidate).toHaveBeenCalled());
		expect(doReleaseEnvironmentMock).toHaveBeenCalledTimes(1);

		fireEvent.click(screen.getByRole("button", { name: "Deploy changes" }));
		await waitFor(() =>
			expect(doReleaseEnvironmentMock).toHaveBeenCalledTimes(2),
		);
		expect(doReleaseEnvironmentMock).toHaveBeenNthCalledWith(2, {
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
		await waitFor(() => expect(routerMock.invalidate).toHaveBeenCalledTimes(2));
	});
});
