// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { PanelSettings } from "./panel-settings";

const { doDeleteServiceMock, doScaleServiceMock, doUpdateServiceMock } =
	vi.hoisted(() => ({
		doDeleteServiceMock: vi.fn(),
		doScaleServiceMock: vi.fn(),
		doUpdateServiceMock: vi.fn(),
	}));

vi.mock("./server-fns", () => ({
	doDeleteService: doDeleteServiceMock,
	doScaleService: doScaleServiceMock,
	doUpdateService: doUpdateServiceMock,
}));

beforeEach(() => {
	doDeleteServiceMock.mockReset().mockResolvedValue(undefined);
	doScaleServiceMock.mockReset();
	doUpdateServiceMock.mockReset();
});

afterEach(cleanup);

describe("PanelSettings", () => {
	it("hides the CPU and memory resource fields", () => {
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		expect(screen.queryByText(/CPU request/i)).toBeNull();
		expect(screen.queryByText(/Memory request/i)).toBeNull();
	});

	it("does not expose sandbox profile variants", () => {
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		expect(screen.queryByLabelText("Sandbox profile")).toBeNull();
		expect(screen.getByText(/same production sandbox/i)).toBeTruthy();
		expect(screen.getByText(/image USER is preserved/i)).toBeTruthy();
		expect(screen.getByText(/SYS_ADMIN/i)).toBeTruthy();
	});

	it("changes the source repository through the picker", async () => {
		const withRepos = state();
		withRepos.repositories = [
			{
				fullName: "octocat/other",
				owner: "octocat",
				name: "other",
				private: false,
				defaultBranch: "main",
			},
		];
		doUpdateServiceMock.mockResolvedValue(service());
		render(
			<PanelSettings
				service={service()}
				state={withRepos}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		expect(screen.getByText("octocat/hello")).toBeTruthy();
		expect(screen.queryByRole("combobox")).toBeNull();

		fireEvent.click(
			screen.getByRole("button", { name: /change source repository/i }),
		);
		fireEvent.click(screen.getByRole("button", { name: "octocat/other" }));

		expect(screen.getByText("octocat/other")).toBeTruthy();
		expect(screen.queryByRole("button", { name: /save changes/i })).toBeNull();

		await waitFor(() =>
			expect(doUpdateServiceMock).toHaveBeenCalledWith({
				data: expect.objectContaining({
					serviceId: "service-1",
					repositorySelector: "octocat/other",
				}),
			}),
		);
	});

	it("deletes the service once its name is typed back", async () => {
		const onDeleted = vi.fn();
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={onDeleted}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: /delete service/i }));

		const confirm = screen.getByRole("button", { name: "Delete" });
		expect((confirm as HTMLButtonElement).disabled).toBe(true);

		fireEvent.change(screen.getByLabelText("Type hello to confirm"), {
			target: { value: "hello" },
		});
		fireEvent.click(confirm);

		await waitFor(() =>
			expect(doDeleteServiceMock).toHaveBeenCalledWith({
				data: { serviceId: "service-1" },
			}),
		);
		expect(onDeleted).toHaveBeenCalledWith("service-1");
	});

	it("keeps the service when the delete request fails", async () => {
		doDeleteServiceMock.mockRejectedValue(new Error("service is protected"));
		const onDeleted = vi.fn();
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={onDeleted}
			/>,
		);

		fireEvent.click(screen.getByRole("button", { name: /delete service/i }));
		fireEvent.change(screen.getByLabelText("Type hello to confirm"), {
			target: { value: "hello" },
		});
		fireEvent.click(screen.getByRole("button", { name: "Delete" }));

		expect(await screen.findByText("service is protected")).toBeTruthy();
		expect(onDeleted).not.toHaveBeenCalled();
	});
});

describe("PanelSettings replica scaling", () => {
	it("queues a replica count without applying it live", async () => {
		const queued = {
			...service(),
			desiredReplicaCount: 1,
			spec: {
				...service().spec,
				desiredReplicaCount: 2,
				runtime: service().spec?.runtime ?? {
					env: {},
					cpuMillis: 250,
					memoryMebibytes: 256,
					ports: [],
				},
			},
			pendingChanges: true,
			unappliedChangeCount: 1,
			unappliedChanges: [
				{
					id: "desiredReplicaCount",
					section: "Replicas",
					field: "Current count",
					path: "desiredReplicaCount",
					action: "update" as const,
					currentValue: "1",
					newValue: "2",
				},
			],
		};
		doScaleServiceMock.mockResolvedValue({ service: queued });
		const onSaved = vi.fn();
		render(
			<PanelSettings
				service={{ ...service(), desiredReplicaCount: 1, readyReplicaCount: 1 }}
				state={state()}
				onSaved={onSaved}
				onDeleted={() => {}}
			/>,
		);

		expect(screen.queryByLabelText("Desired count")).toBeNull();
		expect(screen.queryByRole("button", { name: "Scale up" })).toBeNull();
		expect(screen.queryByText(/1 of 1 ready/i)).toBeNull();

		const input = screen.getByLabelText("Current count");
		fireEvent.change(input, { target: { value: "2" } });
		await waitFor(() => {
			expect(doScaleServiceMock).toHaveBeenCalledWith({
				data: {
					serviceId: "service-1",
					desiredReplicaCount: 2,
				},
			});
		});
		expect(onSaved).toHaveBeenCalledWith(
			expect.objectContaining({
				desiredReplicaCount: 1,
				spec: expect.objectContaining({ desiredReplicaCount: 2 }),
				unappliedChangeCount: 1,
			}),
		);
	});

	it("rejects a second replica on a volume-backed service", () => {
		const current = service();
		const spec = current.spec;
		if (!spec) {
			throw new Error("expected service spec");
		}
		const onSaved = vi.fn();
		render(
			<PanelSettings
				service={{
					...current,
					desiredReplicaCount: 1,
					readyReplicaCount: 1,
					spec: {
						...spec,
						runtime: {
							...spec.runtime,
							volumeName: "data",
						},
					},
				}}
				state={state()}
				onSaved={onSaved}
				onDeleted={() => {}}
			/>,
		);

		const input = screen.getByLabelText("Current count");
		fireEvent.change(input, { target: { value: "2" } });
		expect(doScaleServiceMock).not.toHaveBeenCalled();
		expect(onSaved).not.toHaveBeenCalled();
		expect(
			screen.getAllByText(
				/volume-backed services cannot run more than one replica/i,
			).length,
		).toBeGreaterThan(0);
	});

	it("queues placement region with the rest of the spec", async () => {
		doUpdateServiceMock.mockResolvedValue(service());
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		fireEvent.change(screen.getByLabelText("Required region"), {
			target: { value: "eu-west" },
		});
		await waitFor(() =>
			expect(doUpdateServiceMock).toHaveBeenCalledWith({
				data: expect.objectContaining({
					serviceId: "service-1",
					placementRegion: "eu-west",
				}),
			}),
		);
	});

	it("queues rolling strategy changes with the rest of the spec", async () => {
		doUpdateServiceMock.mockResolvedValue(service());
		render(
			<PanelSettings
				service={service()}
				state={state()}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		fireEvent.change(screen.getByLabelText("Max surge"), {
			target: { value: "2" },
		});
		await waitFor(() =>
			expect(doUpdateServiceMock).toHaveBeenCalledWith({
				data: expect.objectContaining({
					serviceId: "service-1",
					rollingStrategy: expect.objectContaining({
						maxUnavailable: 0,
						maxSurge: 2,
						startupTimeoutSeconds: 300,
						drainTimeoutSeconds: 30,
					}),
				}),
			}),
		);
	});

	it("does not loop when a settings field is queued back into the service", async () => {
		doUpdateServiceMock.mockResolvedValue(service());
		function Harness() {
			const [current, setCurrent] = useState(service());
			return (
				<PanelSettings
					service={current}
					state={state()}
					onSaved={setCurrent}
					onDeleted={() => {}}
				/>
			);
		}
		render(<Harness />);

		const branch = screen.getByLabelText("Branch");
		fireEvent.change(branch, { target: { value: "release" } });
		expect((branch as HTMLInputElement).value).toBe("release");

		await waitFor(() => expect(doUpdateServiceMock).toHaveBeenCalledTimes(1));
		expect(doUpdateServiceMock).toHaveBeenCalledWith({
			data: expect.objectContaining({
				serviceId: "service-1",
				trackedRef: "release",
			}),
		});
		expect((screen.getByLabelText("Branch") as HTMLInputElement).value).toBe(
			"release",
		);
	});

	it("rejects a replica count of zero", () => {
		doUpdateServiceMock.mockResolvedValue(service());
		render(
			<PanelSettings
				service={{ ...service(), desiredReplicaCount: 1, readyReplicaCount: 1 }}
				state={state()}
				onSaved={() => {}}
				onDeleted={() => {}}
			/>,
		);

		const input = screen.getByLabelText("Current count");
		fireEvent.change(input, { target: { value: "0" } });
		expect(doUpdateServiceMock).not.toHaveBeenCalled();
		expect(screen.queryByText("Scale production to zero")).toBeNull();

		fireEvent.blur(input);
		expect(screen.getByText("Enter a whole number from 1 to 64")).toBeTruthy();
		expect(doUpdateServiceMock).not.toHaveBeenCalled();
	});
});

function service(): DashboardServiceRecord {
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
	};
}

function state(): DashboardHomeState {
	const environment = {
		id: "environment-1",
		projectId: "project-1",
		name: "production",
		kind: "persistent" as const,
		isProduction: true,
	};
	return {
		user: { id: "user-1", email: "user@example.com" },
		project: { id: "project-1", name: "test-project", kind: "user" },
		environments: [environment],
		environment,
		onboarding: {
			currentStep: "build",
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: "service-1",
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		services: [service()],
		service: service(),
		serviceStatus: undefined,
		publicBaseURL: "https://dashboard.example.test",
		ingressTargetHost: "platform.example.test",
		domainBindings: [],
		controlPlaneReachable: true,
	};
}
