import { describe, expect, it } from "vitest";
import { buildAllowedDevHosts } from "./vite-dev-hosts";

describe("buildAllowedDevHosts", () => {
	it("builds the host allowlist from defaults and configured endpoints", () => {
		expect(buildAllowedDevHosts()).toEqual(["127.0.0.1", "localhost"]);
		expect(buildAllowedDevHosts("https://mesh.dev.example.test")).toEqual([
			"127.0.0.1",
			"localhost",
			"mesh.dev.example.test",
		]);
		expect(
			buildAllowedDevHosts(
				"https://mesh.dev.example.test",
				"platform.localtest.me",
			),
		).toEqual([
			"127.0.0.1",
			"localhost",
			"mesh.dev.example.test",
			"platform.localtest.me",
		]);
		expect(buildAllowedDevHosts("not-a-url")).toEqual([
			"127.0.0.1",
			"localhost",
			"not-a-url",
		]);
	});
});
