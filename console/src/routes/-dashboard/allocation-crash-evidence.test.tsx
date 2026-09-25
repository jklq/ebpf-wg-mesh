// @vitest-environment jsdom
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DashboardAllocationStatus } from "#/lib/dashboard/core/types.server";
import { AllocationCrashEvidence } from "./allocation-crash-evidence";

const serverFns = vi.hoisted(() => ({ fetchServiceLogs: vi.fn() }));

vi.mock("./server-fns", () => ({
	fetchServiceLogs: serverFns.fetchServiceLogs,
}));

function allocation(
	overrides: Partial<DashboardAllocationStatus>,
): DashboardAllocationStatus {
	return {
		allocationId: "alloc-1",
		serviceId: "svc-1",
		agentId: "agent-1",
		desiredSpecRevision: 1,
		appliedSpecRevision: 1,
		phase: "CrashLoop",
		message: "",
		allocationIpv4: "10.200.0.2",
		allocationIpv6: "fd00:200::2",
		healthy: false,
		desiredRolloutGeneration: 1,
		appliedRolloutGeneration: 1,
		healthyIpv4Ports: [],
		healthyIpv6Ports: [],
		...overrides,
	};
}

describe("allocation crash evidence", () => {
	afterEach(cleanup);

	beforeEach(() => {
		serverFns.fetchServiceLogs.mockReset().mockResolvedValue({
			lines: [
				{
					observedAt: new Date("2026-08-13T10:00:00Z"),
					allocationId: "alloc-1",
					agentId: "agent-1",
					stream: "stderr",
					rolloutGeneration: 1,
					sequence: 1,
					line: "panic: runtime error",
					logType: "SERVICE_LOG_TYPE_RUNTIME",
				},
			],
			gaps: [],
		});
	});

	it("shows OOM cause with exit code, restarts, crash loop, and log tail", async () => {
		render(
			<AllocationCrashEvidence
				serviceId="svc-1"
				allocations={[
					allocation({
						restart: {
							restartCount: 5,
							crashLoop: true,
							lastCause: "RESTART_CAUSE_OOM_KILL",
							message: "crash loop after OOM kill",
							lastExitCode: 137,
							lastSignal: 0,
							awaitingRestart: false,
						},
					}),
				]}
				logsEnabled
			/>,
		);
		expect(await screen.findByText("OOM kill")).toBeTruthy();
		expect(screen.getByText("Crash loop")).toBeTruthy();
		expect(screen.getByText(/exit 137/)).toBeTruthy();
		expect(screen.getByText(/5 restarts/)).toBeTruthy();
		expect(await screen.findByText("panic: runtime error")).toBeTruthy();
		expect(serverFns.fetchServiceLogs).toHaveBeenCalledWith({
			data: expect.objectContaining({
				serviceId: "svc-1",
				allocationId: "alloc-1",
				logType: "SERVICE_LOG_TYPE_RUNTIME",
			}),
		});
	});

	function crashedAllocations(restartOverrides: Record<string, unknown> = {}) {
		return [
			allocation({
				restart: {
					restartCount: 5,
					crashLoop: true,
					lastCause: "RESTART_CAUSE_OOM_KILL",
					message: "crash loop after OOM kill",
					lastExitCode: 137,
					lastSignal: 0,
					awaitingRestart: false,
					...restartOverrides,
				},
			}),
		];
	}

	function crashLogLine() {
		return {
			observedAt: new Date("2026-08-13T10:00:00Z"),
			allocationId: "alloc-1",
			agentId: "agent-1",
			stream: "stderr",
			rolloutGeneration: 1,
			sequence: 1,
			line: "panic: runtime error",
			logType: "SERVICE_LOG_TYPE_RUNTIME",
		};
	}

	it("retries an empty log tail until ingestion lands", async () => {
		vi.useFakeTimers();
		try {
			serverFns.fetchServiceLogs
				.mockReset()
				.mockResolvedValueOnce({ lines: [], gaps: [] })
				.mockResolvedValue({ lines: [crashLogLine()], gaps: [] });
			render(
				<AllocationCrashEvidence
					serviceId="svc-1"
					allocations={crashedAllocations()}
					logsEnabled
				/>,
			);
			await act(async () => {});
			expect(serverFns.fetchServiceLogs).toHaveBeenCalledTimes(1);
			expect(screen.getByText("Loading crash logs…")).toBeTruthy();
			await act(async () => {
				await vi.advanceTimersByTimeAsync(1600);
			});
			expect(screen.getByText("panic: runtime error")).toBeTruthy();
			expect(serverFns.fetchServiceLogs).toHaveBeenCalledTimes(2);
		} finally {
			vi.useRealTimers();
		}
	});

	it("stops retrying an empty log tail after bounded attempts", async () => {
		vi.useFakeTimers();
		try {
			serverFns.fetchServiceLogs
				.mockReset()
				.mockResolvedValue({ lines: [], gaps: [] });
			render(
				<AllocationCrashEvidence
					serviceId="svc-1"
					allocations={crashedAllocations()}
					logsEnabled
				/>,
			);
			await act(async () => {});
			for (let i = 0; i < 5; i++) {
				await act(async () => {
					await vi.advanceTimersByTimeAsync(1600);
				});
			}
			expect(serverFns.fetchServiceLogs).toHaveBeenCalledTimes(5);
			expect(
				screen.getByText("No runtime logs retained for this allocation."),
			).toBeTruthy();
		} finally {
			vi.useRealTimers();
		}
	});

	it("refetches the log tail when the same allocation crashes again", async () => {
		serverFns.fetchServiceLogs
			.mockReset()
			.mockResolvedValueOnce({
				lines: [{ ...crashLogLine(), line: "first crash tail" }],
				gaps: [],
			})
			.mockResolvedValue({
				lines: [{ ...crashLogLine(), line: "second crash tail" }],
				gaps: [],
			});
		const first = render(
			<AllocationCrashEvidence
				serviceId="svc-1"
				allocations={crashedAllocations({
					restartCount: 5,
					lastRestartAt: new Date("2026-08-13T10:00:00Z"),
				})}
				logsEnabled
			/>,
		);
		expect(await screen.findByText("first crash tail")).toBeTruthy();
		expect(serverFns.fetchServiceLogs).toHaveBeenCalledTimes(1);
		first.rerender(
			<AllocationCrashEvidence
				serviceId="svc-1"
				allocations={crashedAllocations({
					restartCount: 6,
					lastRestartAt: new Date("2026-08-13T10:05:00Z"),
					lastExitCode: 1,
				})}
				logsEnabled
			/>,
		);
		expect(await screen.findByText("second crash tail")).toBeTruthy();
		expect(serverFns.fetchServiceLogs).toHaveBeenCalledTimes(2);
		expect(screen.queryByText("first crash tail")).toBeNull();
	});

	it("labels readiness failures as probe-not-ready, not a crash", async () => {
		render(
			<AllocationCrashEvidence
				serviceId="svc-1"
				allocations={[
					allocation({
						phase: "Starting",
						message: "HTTP readiness check not ready: IPv4: connection refused",
						restart: undefined,
					}),
				]}
				logsEnabled
			/>,
		);
		expect(
			(await screen.findAllByText("Probe not ready")).length,
		).toBeGreaterThan(0);
		expect(screen.queryByText("OOM kill")).toBeNull();
	});
});
