import { describe, expect, it } from "vitest";

import { sanitizeRedirect } from "./utils.server";

describe("sanitizeRedirect", () => {
	it.each([
		[undefined, "/"],
		["https://evil.example", "/"],
		["//evil.example", "/"],
		["/\\evil.example", "/"],
		["/dashboard?tab=logs", "/dashboard?tab=logs"],
	])("sanitizes %s", (value, expected) => {
		expect(sanitizeRedirect(value)).toBe(expected);
	});
});
