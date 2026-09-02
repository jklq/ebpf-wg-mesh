import type {
	DashboardBuildRecipe,
	DashboardResolvedSourceBinding,
	DashboardRuntimePort,
	DashboardServiceSourceSummary,
	DashboardServiceSpec,
	DashboardSourceSpec,
} from "#/lib/dashboard/core/types.server";
import {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "#/lib/dashboard/core/types.server";
import type { CreateServiceRequest } from "#/lib/platform-grpc/types.server";
import { decodeSourceAccessState } from "./codec-enums.server";
import {
	readArray,
	readBoolean,
	readOptionalNumber,
	readOptionalNumberLike,
	readOptionalRecord,
	readOptionalString,
	readPositiveResource,
	readStringMap,
} from "./codec-read.server";

export function decodeServiceSpec(
	raw: unknown,
): DashboardServiceSpec | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const runtime = decodeRuntimeSpec(value.runtime);
	const source = decodeServiceSource(value.source);
	if (
		!source &&
		runtime.ports.length === 0 &&
		Object.keys(runtime.env).length === 0
	) {
		return undefined;
	}
	const desiredReplicaCount = readOptionalNumberLike(
		value,
		"desiredReplicaCount",
	);
	const placementRegion = readOptionalString(value, "placementRegion");
	const rollingStrategyValue = readOptionalRecord(value.rollingStrategy);
	const rollingStrategy = rollingStrategyValue
		? {
				maxUnavailable:
					readOptionalNumberLike(rollingStrategyValue, "maxUnavailable") ?? 0,
				maxSurge: readOptionalNumberLike(rollingStrategyValue, "maxSurge") ?? 1,
				startupTimeoutSeconds:
					readOptionalNumberLike(
						rollingStrategyValue,
						"startupTimeoutSeconds",
					) ?? 300,
				drainTimeoutSeconds:
					readOptionalNumberLike(rollingStrategyValue, "drainTimeoutSeconds") ??
					30,
			}
		: undefined;
	return {
		source,
		runtime,
		...(desiredReplicaCount === undefined ? {} : { desiredReplicaCount }),
		...(placementRegion ? { placementRegion } : {}),
		...(rollingStrategy === undefined ? {} : { rollingStrategy }),
	};
}

export function decodeServiceSourceSummary(
	raw: unknown,
): DashboardServiceSourceSummary | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const sourceState = readOptionalRecord(value.sourceState);
	if (!sourceState) {
		return undefined;
	}
	return {
		desiredSpec: decodeSourceSpec(sourceState.desiredSpec),
		resolvedBinding: decodeResolvedSourceBinding(sourceState.resolvedBinding),
	};
}

export function decodeSourceSpec(
	raw: unknown,
): DashboardSourceSpec | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return {
		provider: readOptionalString(value, "provider") ?? "",
		repositorySelector: readOptionalString(value, "repositorySelector") ?? "",
		trackedRef: readOptionalString(value, "trackedRef") ?? "",
		buildRecipe: decodeOptionalBuildRecipe(value.buildRecipe),
	};
}

export function decodeResolvedSourceBinding(
	raw: unknown,
): DashboardResolvedSourceBinding | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return {
		repositorySelector: readOptionalString(value, "repositorySelector") ?? "",
		trackedRef: readOptionalString(value, "trackedRef") ?? "",
		accessState: decodeSourceAccessState(value.accessState),
		buildRecipe: decodeOptionalBuildRecipe(value.buildRecipe),
	};
}

export function encodeBuildRecipe(
	recipe: DashboardBuildRecipe | undefined,
): CreateServiceRequest["service"]["spec"]["source"]["sourceSpec"]["buildRecipe"] {
	if (!recipe) {
		return undefined;
	}
	return {
		dockerfilePath: recipe.dockerfilePath,
		contextDir: recipe.contextDir,
	};
}

export function encodeRuntimeSpec(
	runtime: DashboardServiceSpec["runtime"],
): CreateServiceRequest["service"]["spec"]["runtime"] {
	return {
		env: runtime.env ?? {},
		cpuMillis: runtime.cpuMillis,
		memoryMebibytes: runtime.memoryMebibytes,
		ports: (runtime.ports ?? []).map((port) => ({
			port: port.port,
			primary: port.primary,
		})),
		healthCheck: runtime.healthCheck
			? {
					type: "TYPE_HTTP",
					path: runtime.healthCheck.path,
					port: runtime.healthCheck.port,
					timeoutSeconds: runtime.healthCheck.timeoutSeconds,
				}
			: undefined,
		livenessCheck: runtime.livenessCheck
			? {
					type: "TYPE_HTTP",
					path: runtime.livenessCheck.path,
					port: runtime.livenessCheck.port,
					timeoutSeconds: runtime.livenessCheck.timeoutSeconds,
				}
			: undefined,
		restart: encodeRestartSpec(runtime.restart),
		volumeName: runtime.volumeName,
	};
}

export function encodeRestartSpec(
	restart: DashboardServiceSpec["runtime"]["restart"],
): CreateServiceRequest["service"]["spec"]["runtime"]["restart"] {
	if (!restart) {
		return undefined;
	}
	const policy =
		restart.policy === "always"
			? "RESTART_POLICY_ALWAYS"
			: restart.policy === "never"
				? "RESTART_POLICY_NEVER"
				: "RESTART_POLICY_ON_FAILURE";
	return {
		policy,
		maxRestarts: restart.maxRestarts,
		windowSeconds: restart.windowSeconds,
		initialDelayMs: restart.initialDelayMs,
		maxDelayMs: restart.maxDelayMs,
		backoffMultiplier: restart.backoffMultiplier,
		jitter: restart.jitter,
		stableAfterSeconds: restart.stableAfterSeconds,
	};
}

export function encodeServiceSource(
	source: DashboardSourceSpec | undefined,
): CreateServiceRequest["service"]["spec"]["source"] {
	return {
		sourceSpec: {
			provider: source?.provider ?? "",
			repositorySelector: source?.repositorySelector ?? "",
			trackedRef: source?.trackedRef ?? "",
			buildRecipe: encodeBuildRecipe(source?.buildRecipe),
		},
	};
}

export function decodeRuntimeSpec(
	raw: unknown,
): DashboardServiceSpec["runtime"] {
	const value = readOptionalRecord(raw);
	return {
		env: readStringMap(value, "env"),
		cpuMillis: readPositiveResource(
			value,
			"cpuMillis",
			DEFAULT_SERVICE_CPU_MILLIS,
		),
		memoryMebibytes: readPositiveResource(
			value,
			"memoryMebibytes",
			DEFAULT_SERVICE_MEMORY_MEBIBYTES,
		),
		ports: readArray(value ?? {}, "ports")
			.map((item) => decodeRuntimePort(item))
			.filter((item): item is DashboardRuntimePort => item !== undefined),
		healthCheck: decodeHTTPHealthCheck(value?.healthCheck),
		livenessCheck: decodeHTTPHealthCheck(value?.livenessCheck),
		restart: decodeRestartSpec(value?.restart),
		volumeName: readOptionalString(value, "volumeName") ?? undefined,
	};
}

export function decodeRestartSpec(
	raw: unknown,
): DashboardServiceSpec["runtime"]["restart"] {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const policyRaw = readOptionalString(value, "policy") ?? "";
	const policy =
		policyRaw === "RESTART_POLICY_ALWAYS" || policyRaw === "always"
			? "always"
			: policyRaw === "RESTART_POLICY_NEVER" || policyRaw === "never"
				? "never"
				: "on-failure";
	return {
		policy,
		maxRestarts: readOptionalNumber(value, "maxRestarts"),
		windowSeconds: readOptionalNumber(value, "windowSeconds"),
		initialDelayMs: readOptionalNumber(value, "initialDelayMs"),
		maxDelayMs: readOptionalNumber(value, "maxDelayMs"),
		backoffMultiplier: readOptionalNumber(value, "backoffMultiplier"),
		jitter: readOptionalNumber(value, "jitter"),
		stableAfterSeconds: readOptionalNumber(value, "stableAfterSeconds"),
	};
}

export function decodeHTTPHealthCheck(
	raw: unknown,
): DashboardServiceSpec["runtime"]["healthCheck"] {
	const value = readOptionalRecord(raw);
	if (!value || readOptionalString(value, "type") !== "TYPE_HTTP") {
		return undefined;
	}
	const path = readOptionalString(value, "path") ?? "";
	if (!path) {
		return undefined;
	}
	return {
		path,
		port: readOptionalNumber(value, "port") || undefined,
		timeoutSeconds: readOptionalNumber(value, "timeoutSeconds") || undefined,
	};
}

export function decodeRuntimePort(
	raw: unknown,
): DashboardRuntimePort | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const port = readOptionalNumber(value, "port");
	if (!port || !Number.isInteger(port) || port < 1 || port > 65535) {
		return undefined;
	}
	return {
		port,
		primary: readBoolean(value, "primary"),
	};
}

export function decodeServiceSource(
	raw: unknown,
): DashboardSourceSpec | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	return decodeSourceSpec(value.sourceSpec);
}

export function decodeOptionalBuildRecipe(
	raw: unknown,
): DashboardBuildRecipe | undefined {
	const value = readOptionalRecord(raw);
	if (!value) {
		return undefined;
	}
	const dockerfilePath = readOptionalString(value, "dockerfilePath") ?? "";
	const contextDir = readOptionalString(value, "contextDir") ?? "";
	if (dockerfilePath === "" && contextDir === "") {
		return undefined;
	}
	return {
		dockerfilePath,
		contextDir,
	};
}
