// @vitest-environment jsdom

import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { VolumeSettings } from "#/features/dashboard/service-panel/settings/volume-settings";
import { VolumePanel } from "#/features/dashboard/volumes/volume-panel";
import { jsonFixture, serviceFixture } from "#/lib/dashboard/testkit/protocol";
import { VolumeSchema } from "#/lib/platform-gen/platform_pb";

const { doDeleteResourceMock, doGrowVolumeMock, doUpdateServiceMock } =
	vi.hoisted(() => ({
		doDeleteResourceMock: vi.fn(),
		doGrowVolumeMock: vi.fn(),
		doUpdateServiceMock: vi.fn(),
	}));

vi.mock("#/lib/dashboard/server-functions", () => ({
	doDeleteResource: doDeleteResourceMock,
	doGrowVolume: doGrowVolumeMock,
	doUpdateService: doUpdateServiceMock,
	fetchDeletionPreview: vi.fn(),
}));

beforeEach(() => {
	doDeleteResourceMock.mockReset();
	doGrowVolumeMock.mockReset();
	doUpdateServiceMock.mockReset();
});

afterEach(cleanup);

const GIB = 1024 ** 3;

function volume() {
	return jsonFixture(VolumeSchema, {
		id: "volume-1",
		environmentId: "environment-1",
		name: "pg",
		sizeBytes: String(5 * GIB),
		usedBytes: String(GIB),
		agentId: "agent-1",
		agentName: "node-a",
		state: "VOLUME_STATE_READY",
		observedAt: "2026-10-03T10:00:00Z",
	});
}

function service(mounted: boolean) {
	return serviceFixture({
		id: "service-1",
		environmentId: "environment-1",
		name: "db",
		spec: {
			desiredReplicaCount: 1,
			runtime: mounted
				? { volume: { volumeName: "pg", mountPath: "/var/lib/pg" } }
				: {},
		},
	});
}

function renderPanel(
	overrides: Partial<Parameters<typeof VolumePanel>[0]> = {},
) {
	return render(
		<VolumePanel
			volume={volume()}
			services={[]}
			onClose={() => {}}
			onMount={() => {}}
			onSelectService={() => {}}
			onVolumeUpdated={() => {}}
			onVolumeDeleted={() => {}}
			onServiceUpdated={() => {}}
			{...overrides}
		/>,
	);
}

describe("VolumePanel", () => {
	it("leads with the capacity gauge and keeps the details terse", () => {
		renderPanel({ services: [service(true)] });
		expect(screen.getByText("1 GiB")).toBeTruthy();
		expect(screen.getByText("/ 5 GiB")).toBeTruthy();
		expect(screen.getByText("20%")).toBeTruthy();
		expect(screen.getByText("node-a")).toBeTruthy();
		expect(screen.getByText("/var/lib/pg")).toBeTruthy();
		expect(screen.queryByText(/if that node is lost/i)).toBeNull();
		expect(
			(
				screen.getByRole("button", {
					name: /delete volume/i,
				}) as HTMLButtonElement
			).disabled,
		).toBe(true);
	});

	it("previews a grow step on the gauge before growing", async () => {
		const onVolumeUpdated = vi.fn();
		doGrowVolumeMock.mockResolvedValue({
			...volume(),
			sizeBytes: String(10 * GIB),
		});
		renderPanel({ onVolumeUpdated });

		fireEvent.click(screen.getByRole("button", { name: "+5" }));
		expect(screen.getByText("+5 GiB → 10 GiB")).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: "Grow to 10 GiB" }));
		await waitFor(() =>
			expect(doGrowVolumeMock).toHaveBeenCalledWith({
				data: { volumeId: "volume-1", sizeBytes: 10 * GIB },
			}),
		);
		await waitFor(() => expect(onVolumeUpdated).toHaveBeenCalled());
	});

	it("refuses a custom size below the current one", () => {
		renderPanel();
		fireEvent.change(screen.getByLabelText("Size (GiB)"), {
			target: { value: "4" },
		});
		expect(screen.getByText(/at least 5 GiB/)).toBeTruthy();
		expect(screen.queryByRole("button", { name: /grow to/i })).toBeNull();
	});

	it("discards a staged volume without a confirmation dialog", async () => {
		const onVolumeDeleted = vi.fn();
		doDeleteResourceMock.mockResolvedValue(undefined);
		renderPanel({
			volume: { ...volume(), staged: true, agentId: "", agentName: "" },
			onVolumeDeleted,
		});
		expect(screen.getByText("New")).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: /discard volume/i }));
		await waitFor(() =>
			expect(doDeleteResourceMock).toHaveBeenCalledWith({
				data: { kind: "volume", id: "volume-1", confirmationName: undefined },
			}),
		);
		await waitFor(() =>
			expect(onVolumeDeleted).toHaveBeenCalledWith("volume-1"),
		);
	});
});

describe("VolumeSettings", () => {
	it("attaches an unmounted volume at the chosen path", async () => {
		const onSaved = vi.fn();
		doUpdateServiceMock.mockResolvedValue(service(true));
		render(
			<VolumeSettings
				service={service(false)}
				services={[service(false)]}
				volumes={[volume()]}
				onSaved={onSaved}
			/>,
		);
		fireEvent.change(screen.getByLabelText("Mount path"), {
			target: { value: "/var/lib/pg" },
		});
		fireEvent.click(screen.getByRole("button", { name: "Attach" }));
		await waitFor(() =>
			expect(doUpdateServiceMock).toHaveBeenCalledWith({
				data: {
					serviceId: "service-1",
					volumeMount: { volumeName: "pg", mountPath: "/var/lib/pg" },
				},
			}),
		);
		await waitFor(() => expect(onSaved).toHaveBeenCalled());
	});

	it("rejects system mount paths before saving", () => {
		render(
			<VolumeSettings
				service={service(false)}
				services={[service(false)]}
				volumes={[volume()]}
				onSaved={() => {}}
			/>,
		);
		fireEvent.change(screen.getByLabelText("Mount path"), {
			target: { value: "/etc/pg" },
		});
		expect(screen.getByText(/system directory \/etc/)).toBeTruthy();
		expect(
			(screen.getByRole("button", { name: "Attach" }) as HTMLButtonElement)
				.disabled,
		).toBe(true);
	});

	it("detaches the mounted volume", async () => {
		doUpdateServiceMock.mockResolvedValue(service(false));
		render(
			<VolumeSettings
				service={service(true)}
				services={[service(true)]}
				volumes={[volume()]}
				onSaved={() => {}}
			/>,
		);
		fireEvent.click(screen.getByRole("button", { name: /detach/i }));
		await waitFor(() =>
			expect(doUpdateServiceMock).toHaveBeenCalledWith({
				data: { serviceId: "service-1", volumeMount: null },
			}),
		);
	});
});
