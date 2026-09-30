import type { DescMessage, MessageJsonType } from "@bufbuild/protobuf";
import type {
	DashboardServicePosition,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import {
	type ServiceJson,
	ServiceSchema,
	type ServiceStatusJson,
	ServiceStatusSchema,
} from "#/lib/platform-gen/platform_pb";
import {
	fromPlatformJson,
	type PlatformJson,
	toPlatformJson,
} from "#/lib/platform-json";

/** Fixtures use the same canonical codec and defaults as production RPCs. */
export function jsonFixture<D extends DescMessage>(
	schema: D,
	input: MessageJsonType<D>,
): PlatformJson<D> {
	return toPlatformJson(schema, fromPlatformJson(schema, input));
}

export function serviceFixture(
	input: ServiceJson & {
		projectId?: string;
		layoutPosition?: DashboardServicePosition;
	},
): DashboardServiceRecord {
	const { projectId, layoutPosition, ...service } = input;
	return {
		...jsonFixture(ServiceSchema, service),
		...(projectId === undefined ? {} : { projectId }),
		...(layoutPosition === undefined ? {} : { layoutPosition }),
	};
}

/** Status embeds a protocol service, without the console's canvas metadata. */
export function statusFixture(
	input: ServiceStatusJson,
): PlatformJson<typeof ServiceStatusSchema> {
	const { service, ...status } = input;
	if (!service) return jsonFixture(ServiceStatusSchema, status);
	const {
		projectId: _,
		layoutPosition: __,
		...resource
	} = serviceFixture(service);
	return jsonFixture(ServiceStatusSchema, { ...status, service: resource });
}
