import { describe, expect, it } from "vitest";

import {
	normalizeRepositorySelector,
	recommendedBuildRecipeFromCandidates,
	slugifyServiceName,
} from "#/lib/onboarding-flow";

describe("onboarding flow helpers", () => {
	it("normalizes repository selectors", () => {
		expect(normalizeRepositorySelector(" OctoCat / Hello ")).toBe(
			"octocat/hello",
		);
	});

	it("rejects invalid repository selectors", () => {
		expect(() => normalizeRepositorySelector("octocat")).toThrow(
			"Repository must be in owner/repo form.",
		);
	});

	it("prefers a root Dockerfile when multiple candidates are present", () => {
		expect(
			recommendedBuildRecipeFromCandidates([
				"deploy/Dockerfile",
				"Dockerfile",
				"worker/Dockerfile",
			]),
		).toEqual({
			dockerfilePath: "Dockerfile",
			contextDir: ".",
		});
	});

	it("falls back to the first lexical Dockerfile candidate", () => {
		expect(
			recommendedBuildRecipeFromCandidates([
				"zeta/Dockerfile",
				"apps/api/Dockerfile",
			]),
		).toEqual({
			dockerfilePath: "apps/api/Dockerfile",
			contextDir: "apps/api",
		});
	});

	it("returns undefined when no Dockerfiles are detected", () => {
		expect(recommendedBuildRecipeFromCandidates([])).toBeUndefined();
	});

	it("slugifies service names from repository basenames", () => {
		expect(slugifyServiceName("Hello World")).toBe("hello-world");
	});
});
