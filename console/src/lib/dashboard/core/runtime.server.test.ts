import { describe, expect, it, vi } from "vitest";

import { storeCall } from "#/lib/dashboard/core/runtime.server";
import { createDashboardTestHarness } from "#/lib/dashboard/testkit/harness.server";

describe("store initialization caching", () => {
	it("retries after a failed initialization and caches success", async () => {
		const harness = createDashboardTestHarness();
		let attempts = 0;
		harness.store.ensureInitialized = vi.fn(async () => {
			attempts += 1;
			if (attempts === 1) {
				throw new Error("initialization failed");
			}
		});

		await expect(
			storeCall(harness.runtime, "test.first", async () => "first"),
		).rejects.toThrow();
		expect(harness.runtime.storeInitPromise).toBeUndefined();

		await expect(
			storeCall(harness.runtime, "test.second", async () => "second"),
		).resolves.toBe("second");
		await expect(
			storeCall(harness.runtime, "test.third", async () => "third"),
		).resolves.toBe("third");

		expect(attempts).toBe(2);
	});
});
