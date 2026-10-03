import { describe, expect, it } from "vitest";
import {
	mountPathError,
	suggestVolumeName,
	unattachedVolumes,
	volumeNameError,
	volumeTone,
	volumeUsage,
} from "#/features/dashboard/volumes/volume-model";
import { jsonFixture, serviceFixture } from "#/lib/dashboard/testkit/protocol";
import { VolumeSchema } from "#/lib/platform-gen/platform_pb";

function volume(name: string, extra: Record<string, unknown> = {}) {
	return jsonFixture(VolumeSchema, {
		id: `volume-${name}`,
		environmentId: "environment-1",
		name,
		sizeBytes: String(1024 ** 3),
		...extra,
	});
}

describe("volume model", () => {
	it("accepts clean absolute mount paths and rejects system locations", () => {
		expect(mountPathError("/data")).toBeUndefined();
		expect(mountPathError("/var/lib/postgresql/data")).toBeUndefined();
		expect(mountPathError("data")).toMatch(/absolute/);
		expect(mountPathError("/")).toMatch(/root/);
		expect(mountPathError("/etc/app")).toMatch(/\/etc/);
		expect(mountPathError("/var/run")).toMatch(/\/var\/run/);
		expect(mountPathError("/data/")).toMatch(/clean/);
		expect(mountPathError("/data/../etc")).toMatch(/clean/);
		expect(mountPathError("/my data")).toMatch(/unsupported/);
	});

	it("validates volume names like the control plane", () => {
		expect(volumeNameError("pg-data")).toBeUndefined();
		expect(volumeNameError("")).toBeDefined();
		expect(volumeNameError("PG")).toBeDefined();
		expect(volumeNameError("-data")).toBeDefined();
	});

	it("suggests a name that is not taken", () => {
		const values = [0, 0, 0.5, 0.5];
		let call = 0;
		const random = () => values[call++] ?? 0;
		expect(suggestVolumeName(["amber-acorn"], random)).not.toBe("amber-acorn");
	});

	it("lists volumes no service draft mounts", () => {
		const services = [
			serviceFixture({
				id: "service-1",
				name: "db",
				spec: {
					runtime: { volume: { volumeName: "pg", mountPath: "/data" } },
				},
			}),
		];
		expect(
			unattachedVolumes([volume("pg"), volume("cache")], services).map(
				(entry) => entry.name,
			),
		).toEqual(["cache"]);
	});

	it("reports usage and tone from the observed state", () => {
		const full = volume("pg", {
			usedBytes: String(1024 ** 3),
			state: "VOLUME_STATE_FULL",
		});
		expect(volumeUsage(full).ratio).toBe(1);
		expect(volumeTone(full)).toBe("failed");
		expect(volumeTone(volume("new", { state: "VOLUME_STATE_PENDING" }))).toBe(
			"building",
		);
	});
});
