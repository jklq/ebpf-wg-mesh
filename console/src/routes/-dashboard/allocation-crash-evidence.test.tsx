// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
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
		serverFns.fetchServiceLogs.mockReset().mockResolvedValue([
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
		]);
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
