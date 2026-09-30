import { describe, expect, it } from "vitest";
import {
	parseServicesSnapshot,
	parseStatusSnapshot,
} from "#/features/dashboard/state/dashboard-snapshots";

describe("dashboard SSE protocol", () => {
	it("preserves protocol precision and presence together with UI layout fields", () => {
		const snapshot = parseServicesSnapshot(
			JSON.parse(
				JSON.stringify({
					revision: "9007199254740993",
					services: [
						{
							id: "service-1",
							projectId: "project-1",
							layoutPosition: { x: 12, y: 34 },
							createdAt: "2026-09-30T00:00:00.123456789Z",
							spec: {
								desiredReplicaCount: 0,
								source: { image: {} },
								runtime: { cpuMillis: "9007199254740993" },
							},
						},
					],
				}),
			),
		);
		expect(snapshot.revision).toBe("9007199254740993");
		expect(snapshot.services[0]).toMatchObject({
			projectId: "project-1",
			layoutPosition: { x: 12, y: 34 },
			createdAt: "2026-09-30T00:00:00.123456789Z",
			spec: {
				desiredReplicaCount: 0,
				source: { image: { image: "" } },
				runtime: { cpuMillis: "9007199254740993", ports: [] },
			},
		});
		expect(snapshot.services[0].deletion).toBeUndefined();
	});

	it("requires exact string revisions and a service in status snapshots", () => {
		expect(() =>
			parseServicesSnapshot({ services: [], revision: 9007199254740992 }),
		).toThrow();
		expect(() => parseStatusSnapshot({ status: {}, revision: "1" })).toThrow(
			"missing its service",
		);
		const snapshot = parseStatusSnapshot({
			status: { service: { id: "service-1" } },
			revision: "1",
		});
		expect(snapshot.status.allocations).toEqual([]);
		expect(snapshot.status.service?.spec).toBeUndefined();
	});

	it("rejects malformed protocol fields rather than asserting a dashboard type", () => {
		expect(() =>
			parseServicesSnapshot({
				services: [{ createdAt: "yesterday" }],
				revision: "1",
			}),
		).toThrow();
		expect(() =>
			parseServicesSnapshot({
				services: [{ spec: { source: { image: {}, sourceSpec: {} } } }],
				revision: "1",
			}),
		).toThrow();
	});
});
