import { create, fromJsonString } from "@bufbuild/protobuf";
import { describe, expect, expectTypeOf, it } from "vitest";
import * as P from "#/lib/platform-gen/platform_pb";
import {
	compareIntegers,
	fromPlatformJson,
	integerString,
	safeInteger,
	toPlatformJson,
} from "./platform-json";

describe("canonical platform JSON", () => {
	it("derives required scalar, list and map defaults at every present message level", () => {
		const json = toPlatformJson(
			P.ServiceSchema,
			create(P.ServiceSchema, {
				spec: {
					runtime: { ports: [{ port: 8080 }], env: { NODE_ENV: "production" } },
				},
				latestBuild: {},
			}),
		);
		expect(json.spec?.runtime).toMatchObject({
			command: [],
			args: [],
			env: { NODE_ENV: "production" },
			cpuMillis: "0",
			memoryMebibytes: "0",
			ports: [{ port: 8080, primary: false }],
			volumeName: "",
		});
		expect(json.latestBuild).toMatchObject({
			state: "BUILD_STATE_UNSPECIFIED",
			stages: [],
			attemptCount: "0",
		});
		expectTypeOf(json.id).toEqualTypeOf<string>();
		if (json.spec?.runtime) {
			expectTypeOf(json.spec.runtime.ports).toBeArray();
			expectTypeOf(json.spec.runtime.ports[0].primary).toEqualTypeOf<boolean>();
			expectTypeOf(json.spec.runtime.env.NODE_ENV).toEqualTypeOf<string>();
		}
	});

	it("preserves explicit optional zero scalars and zero-valued oneof messages", () => {
		const absent = toPlatformJson(
			P.ServiceSpecSchema,
			create(P.ServiceSpecSchema),
		);
		expect(absent.desiredReplicaCount).toBeUndefined();
		expect(absent.source).toBeUndefined();
		const selected = toPlatformJson(
			P.ServiceSpecSchema,
			create(P.ServiceSpecSchema, {
				desiredReplicaCount: 0,
				source: { source: { case: "image", value: {} } },
			}),
		);
		expect(selected.desiredReplicaCount).toBe(0);
		expect(selected.source).toEqual({ image: { image: "" } });
		expect(
			fromPlatformJson(P.ServiceSpecSchema, selected).source?.source.case,
		).toBe("image");
	});

	it("preserves nested repository source defaults while keeping other oneofs absent", () => {
		const json = toPlatformJson(
			P.ServiceSchema,
			create(P.ServiceSchema, {
				spec: { source: { source: { case: "sourceSpec", value: {} } } },
				sourceSummary: {
					source: { case: "sourceState", value: { latestRevision: {} } },
				},
			}),
		);
		expect(json.spec?.source?.sourceSpec).toEqual({
			provider: "",
			repositorySelector: "",
			trackedRef: "",
		});
		expect(json.sourceSummary?.sourceState?.latestRevision).toMatchObject({
			id: "",
			commitSha: "",
		});
		if (json.sourceSummary?.sourceState?.latestRevision)
			expectTypeOf(
				json.sourceSummary.sourceState.latestRevision.commitSha,
			).toEqualTypeOf<string>();
		expect(
			toPlatformJson(P.ServiceSourceSchema, create(P.ServiceSourceSchema)),
		).toEqual({});
	});

	it("keeps signed/unsigned int64 limits exact in JSON and ordering", () => {
		const json = toPlatformJson(
			P.ServiceLogLineSchema,
			create(P.ServiceLogLineSchema, {
				sequence: 18446744073709551615n,
				rolloutGeneration: 9223372036854775807n,
			}),
		);
		expect(json.sequence).toBe("18446744073709551615");
		expect(json.rolloutGeneration).toBe("9223372036854775807");
		expect(fromPlatformJson(P.ServiceLogLineSchema, json).sequence).toBe(
			18446744073709551615n,
		);
		expect(compareIntegers("9007199254740992", "9007199254740993")).toBe(-1);
		expect(() => safeInteger("9007199254740993")).toThrow("safe range");
		expect(() => integerString(9007199254740992)).toThrow("Invalid integer");
		expect(safeInteger("9007199254740991")).toBe(Number.MAX_SAFE_INTEGER);
	});

	it("validates generated enums, timestamps, and oneof exclusivity on input", () => {
		expect(() =>
			fromJsonString(P.ProjectSchema, '{"kind":"invalid"}'),
		).toThrow();
		expect(() =>
			fromPlatformJson(P.ProjectSchema, { createdAt: "not a timestamp" }),
		).toThrow();
		expect(() =>
			fromPlatformJson(P.ServiceSourceSchema, { image: {}, sourceSpec: {} }),
		).toThrow();
	});
});
