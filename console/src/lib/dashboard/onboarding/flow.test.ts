import { describe, expect, it } from "vitest";

import {
	generatedServiceNameFromSeed,
	normalizeRepositorySelector,
	slugifyServiceName,
	uniqueServiceName,
} from "#/lib/dashboard/onboarding/flow";

describe("onboarding flow helpers", () => {
	it("normalizes and validates repository selectors", () => {
		expect(normalizeRepositorySelector(" OctoCat / Hello ")).toBe(
			"octocat/hello",
		);
		expect(() => normalizeRepositorySelector("octocat")).toThrow(
			"Repository must be in owner/repo form.",
		);
	});

	it("generates usable and unique service names", () => {
		expect(slugifyServiceName("Hello World")).toBe("hello-world");
		expect(generatedServiceNameFromSeed("seed-1")).toMatch(/^[a-z]+-[a-z]+$/);
		expect(generatedServiceNameFromSeed("seed-1")).toBe(
			generatedServiceNameFromSeed("seed-1"),
		);
		expect(uniqueServiceName(["talented-harmony"], "talented-harmony")).toBe(
			"talented-harmony-2",
		);
	});
});
