import { describe, expect, it } from "vitest";

import { normalizeHostname } from "#/lib/dashboard/domain/dns.server";

describe("domain hostname normalization", () => {
	it("rejects invalid hostnames", () => {
		expect(() => normalizeHostname("https://example.com/path")).toThrow(
			"Enter only a hostname, without a URL scheme.",
		);
		expect(() => normalizeHostname("localhost")).toThrow(
			"Enter a fully qualified hostname.",
		);
		expect(() => normalizeHostname("127.0.0.1")).toThrow(
			"IP addresses are not valid hostnames here.",
		);
	});
});
