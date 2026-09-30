import { fromJsonString } from "@bufbuild/protobuf";
import { z } from "zod";
import {
	ServiceSchema,
	ServiceStatusSchema,
} from "#/lib/platform-gen/platform_pb";
import { toPlatformJson } from "#/lib/platform-json";

const revision = z.string().regex(/^(0|[1-9]\d*)$/);
const service = z
	.looseObject({
		projectId: z.string().optional(),
		layoutPosition: z
			.object({ x: z.number().finite(), y: z.number().finite() })
			.optional(),
	})
	.transform(({ projectId, layoutPosition, ...resource }) => ({
		...toPlatformJson(
			ServiceSchema,
			fromJsonString(ServiceSchema, JSON.stringify(resource)),
		),
		projectId,
		layoutPosition,
	}));

/** SSE uses one envelope and the same protocol JSON as server functions. */
export function parseServicesSnapshot(raw: unknown) {
	return z.object({ services: z.array(service), revision }).parse(raw);
}

export function parseStatusSnapshot(raw: unknown) {
	const envelope = z.object({ status: z.unknown(), revision }).parse(raw);
	const status = toPlatformJson(
		ServiceStatusSchema,
		fromJsonString(ServiceStatusSchema, JSON.stringify(envelope.status)),
	);
	if (!status.service) throw new Error("Service status is missing its service");
	return { status, revision: envelope.revision };
}
