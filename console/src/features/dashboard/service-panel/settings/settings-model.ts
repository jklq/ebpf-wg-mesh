import type {
	DashboardRestartPolicy,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

export type SettingsDraft = {
	repoSelector: string;
	trackedRef: string;
	builder: "railpack" | "dockerfile";
	dockerfilePath: string;
	contextDir: string;
	restartPolicy: "on-failure" | "always" | "never";
	maxRestarts: string;
	windowSeconds: string;
	placementRegion: string;
	healthcheckTimeoutSeconds: string;
	drainingSeconds: string;
};
export function settingsChangedFields(
	service: DashboardServiceRecord,
	incoming: SettingsDraft,
	draft: SettingsDraft,
): Set<string> {
	const changed = new Set(
		(service.unappliedChanges ?? []).map((change) => change.id),
	);
	if (draft.repoSelector !== incoming.repoSelector) {
		changed.add("source.repositorySelector");
	}
	if (draft.trackedRef !== incoming.trackedRef) {
		changed.add("source.trackedRef");
	}
	if (draft.builder !== incoming.builder) {
		changed.add("source.buildRecipe.builder");
	}
	if (draft.dockerfilePath !== incoming.dockerfilePath) {
		changed.add("source.buildRecipe.dockerfilePath");
	}
	if (draft.contextDir !== incoming.contextDir) {
		changed.add("source.buildRecipe.contextDir");
	}
	if (draft.placementRegion !== incoming.placementRegion) {
		changed.add("placementRegion");
	}
	if (
		draft.healthcheckTimeoutSeconds !== incoming.healthcheckTimeoutSeconds ||
		draft.drainingSeconds !== incoming.drainingSeconds
	) {
		changed.add("rollingStrategy");
	}
	if (
		draft.restartPolicy !== incoming.restartPolicy ||
		draft.maxRestarts !== incoming.maxRestarts ||
		draft.windowSeconds !== incoming.windowSeconds
	) {
		changed.add("runtime.restart");
	}
	return changed;
}
export function settingsDraftFromService(
	service: DashboardServiceRecord,
): SettingsDraft {
	const source = service.spec?.source?.sourceSpec;
	return {
		repoSelector: source?.repositorySelector ?? "",
		trackedRef: source?.trackedRef ?? "",
		builder:
			source?.buildRecipe?.builder === "BUILDER_KIND_DOCKERFILE"
				? "dockerfile"
				: "railpack",
		dockerfilePath: source?.buildRecipe?.dockerfilePath ?? "",
		contextDir: source?.buildRecipe?.contextDir ?? ".",
		restartPolicy: restartPolicyToDraft(service.spec?.runtime?.restart?.policy),
		maxRestarts: String(service.spec?.runtime?.restart?.maxRestarts ?? 5),
		windowSeconds: String(service.spec?.runtime?.restart?.windowSeconds ?? 300),
		placementRegion: service.spec?.placementRegion ?? "",
		healthcheckTimeoutSeconds: String(
			service.spec?.rollingStrategy?.healthcheckTimeoutSeconds ?? 300,
		),
		drainingSeconds: String(
			service.spec?.rollingStrategy?.drainingSeconds ?? 0,
		),
	};
}
export function builderToProto(
	builder: SettingsDraft["builder"],
): "BUILDER_KIND_RAILPACK" | "BUILDER_KIND_DOCKERFILE" {
	return builder === "dockerfile"
		? "BUILDER_KIND_DOCKERFILE"
		: "BUILDER_KIND_RAILPACK";
}
export function restartPolicyFromDraft(
	policy: SettingsDraft["restartPolicy"],
):
	| "RESTART_POLICY_ALWAYS"
	| "RESTART_POLICY_NEVER"
	| "RESTART_POLICY_ON_FAILURE" {
	switch (policy) {
		case "always":
			return "RESTART_POLICY_ALWAYS";
		case "never":
			return "RESTART_POLICY_NEVER";
		case "on-failure":
			return "RESTART_POLICY_ON_FAILURE";
	}
}
export function restartPolicyToDraft(
	policy: DashboardRestartPolicy | undefined,
): SettingsDraft["restartPolicy"] {
	switch (policy) {
		case "RESTART_POLICY_ALWAYS":
			return "always";
		case "RESTART_POLICY_NEVER":
			return "never";
		default:
			return "on-failure";
	}
}

export interface SettingsEditor {
	draft: SettingsDraft;
	changedFields: ReadonlySet<string>;
	updateDraft: (patch: Partial<SettingsDraft>) => void;
}
