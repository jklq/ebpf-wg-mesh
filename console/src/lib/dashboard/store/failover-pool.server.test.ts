import { Pool, type PoolClient } from "pg";
import { afterEach, expect, it, vi } from "vitest";
import { FailoverPool } from "./failover-pool.server";

afterEach(() => vi.restoreAllMocks());

it("tries each database endpoint once even when another connection changes the preferred endpoint", async () => {
	const pool = new FailoverPool(["postgres://one/db", "postgres://two/db"]);
	const client = { release: vi.fn() } as unknown as PoolClient;
	let rejectFirst!: (error: Error) => void;
	const firstAttempt = new Promise<PoolClient>((_resolve, reject) => {
		rejectFirst = reject;
	});
	const attempts: string[] = [];
	vi.spyOn(Pool.prototype, "connect").mockImplementation(function (this: Pool) {
		const host = this.options.host ?? "";
		attempts.push(host);
		if (attempts.length === 1) return firstAttempt;
		return host === "two"
			? Promise.resolve(client)
			: Promise.reject(new Error("endpoint unavailable"));
	});
	try {
		const first = pool.connect();
		expect(await pool.connect()).toBe(client);
		rejectFirst(new Error("endpoint unavailable"));
		expect(await first).toBe(client);
		expect(attempts).toEqual(["one", "one", "two", "two"]);
	} finally {
		await pool.end();
	}
});
