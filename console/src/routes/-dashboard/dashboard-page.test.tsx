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

const { doSaveServicePositionMock, routerMock } = vi.hoisted(() => ({
	doSaveServicePositionMock: vi.fn(),
	routerMock: { invalidate: vi.fn() },
}));

vi.mock("@tanstack/react-router", async (importOriginal) => ({
	...(await importOriginal<typeof import("@tanstack/react-router")>()),
	useRouter: () => routerMock,
}));

vi.mock("./server-fns", () => ({
	doDiscardServiceChanges: vi.fn(),
	doRedeployService: vi.fn(),
	doSaveServicePosition: doSaveServicePositionMock,
}));

class MockEventSource {
	static instances: MockEventSource[] = [];

	readonly url: string;
	readonly close = vi.fn();
	readonly addEventListener = vi.fn();
	onerror: (() => void) | null = null;

	constructor(url: string) {
		this.url = url;
		MockEventSource.instances.push(this);
	}
}

beforeEach(() => {
	MockEventSource.instances = [];
	doSaveServicePositionMock.mockReset();
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

		fireEvent.click(screen.getByRole("button", { name: /hello/i }));

		await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
		expect(MockEventSource.instances[0]?.url).toBe(
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
		expect(MockEventSource.instances).toHaveLength(1);
		expect(MockEventSource.instances[0]?.close).not.toHaveBeenCalled();
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

	it("centers the initial viewport around services", () => {
		const { container } = render(
			<DashboardPage state={dashboardState(serviceRecord())} />,
		);

		const world = container.querySelector(".canvas-world") as HTMLElement;

		expect(world.style.transform).toBe("translate(244px,208px) scale(1)");
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
