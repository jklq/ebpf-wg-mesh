import { expect, it } from "vitest";
import { envNameError, envValueError } from "./env-variables";

it("uses the backend name and UTF-8 byte limits", () => {
	for (const name of [
		"",
		"1TOKEN",
		"HAS-DASH",
		"PLATFORM_TOKEN",
		"a".repeat(129),
	])
		expect(envNameError(name)).toBeDefined();
	expect(envNameError("_TOKEN1")).toBeUndefined();
	expect(envValueError("A", "é".repeat(32768))).toBeUndefined();
	expect(envValueError("A", "é".repeat(32769))).toBeDefined();
});
