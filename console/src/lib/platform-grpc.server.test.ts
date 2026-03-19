import { describe, expect, it } from "vitest";

import {
	decodeProjectKind,
	decodeProjectMessage,
} from "#/lib/platform-grpc.server";

describe("platform grpc adapter", () => {
	it("normalizes proto enum labels into dashboard project kinds", () => {
		expect(decodeProjectKind("PROJECT_KIND_USER")).toBe("user");
		expect(decodeProjectKind("PROJECT_KIND_MANAGED")).toBe("managed");
	});

	it("decodes project responses into the dashboard shape", () => {
		expect(
			decodeProjectMessage({
				id: "project-1",
				name: "demo",
				kind: "PROJECT_KIND_MANAGED",
				systemKey: "managed/dashboard",
			}),
		).toEqual({
			id: "project-1",
			name: "demo",
			kind: "managed",
			systemKey: "managed/dashboard",
		});
	});

	it("rejects unknown enum values instead of silently widening the type", () => {
		expect(() => decodeProjectKind("PROJECT_KIND_UNSPECIFIED")).toThrow(
			"invalid project kind",
		);
	});
});
