import type {
	DashboardAgentLifecycleState,
	DashboardBuildState,
	DashboardDeploymentAction,
	DashboardDeploymentCauseKind,
	DashboardDeploymentStageState,
	DashboardDeploymentState,
	DashboardDomainBinding,
	DashboardProject,
	DashboardServiceLogType,
	DashboardUnappliedChangeAction,
	RepositoryAccessState,
} from "#/lib/dashboard/core/types.server";

export function decodeProjectKind(raw: unknown): DashboardProject["kind"] {
	switch (raw) {
		case "PROJECT_KIND_USER":
		case "user":
			return "user";
		case "PROJECT_KIND_MANAGED":
		case "managed":
			return "managed";
		default:
			throw new Error(`invalid project kind: ${String(raw)}`);
	}
}

export function decodeSourceAccessState(raw: unknown): RepositoryAccessState {
	switch (raw) {
		case "SOURCE_ACCESS_STATE_AVAILABLE":
		case "available":
			return "available";
		case "SOURCE_ACCESS_STATE_INSTALLATION_REQUIRED":
		case "installation_required":
			return "installation_required";
		case "SOURCE_ACCESS_STATE_ACCESS_REVOKED":
		case "access_revoked":
			return "access_revoked";
		case "SOURCE_ACCESS_STATE_REPOSITORY_DELETED":
		case "repository_deleted":
			return "repository_deleted";
		default:
			return "unspecified";
	}
}

export function decodeBuildState(raw: unknown): DashboardBuildState {
	switch (raw) {
		case "BUILD_STATE_QUEUED":
		case "queued":
			return "queued";
		case "BUILD_STATE_RUNNING":
		case "running":
			return "running";
		case "BUILD_STATE_SUCCEEDED":
		case "succeeded":
			return "succeeded";
		case "BUILD_STATE_FAILED":
		case "failed":
			return "failed";
		case "BUILD_STATE_SUPERSEDED":
		case "superseded":
			return "superseded";
		case "BUILD_STATE_CANCELLED":
		case "cancelled":
			return "cancelled";
		default:
			return "unspecified";
	}
}

export function decodeDeploymentStageState(
	raw: unknown,
): DashboardDeploymentStageState {
	switch (raw) {
		case "DEPLOYMENT_STAGE_STATE_PENDING":
		case "pending":
			return "pending";
		case "DEPLOYMENT_STAGE_STATE_RUNNING":
		case "running":
			return "running";
		case "DEPLOYMENT_STAGE_STATE_SUCCEEDED":
		case "succeeded":
			return "succeeded";
		case "DEPLOYMENT_STAGE_STATE_FAILED":
		case "failed":
			return "failed";
		case "DEPLOYMENT_STAGE_STATE_SKIPPED":
		case "skipped":
			return "skipped";
		default:
			return "unspecified";
	}
}

export function decodeServiceLogType(raw: unknown): DashboardServiceLogType {
	switch (raw) {
		case "SERVICE_LOG_TYPE_RUNTIME":
		case "runtime":
			return "runtime";
		case "SERVICE_LOG_TYPE_BUILD":
		case "build":
			return "build";
		case "SERVICE_LOG_TYPE_DEPLOY":
		case "deploy":
			return "deploy";
		case "SERVICE_LOG_TYPE_HTTP":
		case "http":
			return "http";
		case "SERVICE_LOG_TYPE_NETWORK":
		case "network":
			return "network";
		default:
			return "unspecified";
	}
}

export function decodeUnappliedChangeAction(
	raw: unknown,
): DashboardUnappliedChangeAction {
	switch (raw) {
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD":
		case 1:
			return "add";
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE":
		case 2:
			return "update";
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE":
		case 3:
			return "remove";
		default:
			return "unspecified";
	}
}

export function decodeAgentLifecycleState(
	raw: unknown,
): DashboardAgentLifecycleState {
	switch (raw) {
		case "AGENT_LIFECYCLE_STATE_ENROLLING":
		case "enrolling":
			return "enrolling";
		case "AGENT_LIFECYCLE_STATE_ACTIVE":
		case "active":
			return "active";
		case "AGENT_LIFECYCLE_STATE_CORDONED":
		case "cordoned":
			return "cordoned";
		case "AGENT_LIFECYCLE_STATE_DRAINING":
		case "draining":
			return "draining";
		case "AGENT_LIFECYCLE_STATE_UNAVAILABLE":
		case "unavailable":
			return "unavailable";
		case "AGENT_LIFECYCLE_STATE_RETIRED":
		case "retired":
			return "retired";
		default:
			return "unspecified";
	}
}

export function decodeDeploymentAction(
	raw: unknown,
): DashboardDeploymentAction {
	switch (raw) {
		case "DEPLOYMENT_ACTION_RESTART":
		case "restart":
			return "restart";
		case "DEPLOYMENT_ACTION_EXACT_REDEPLOY":
		case "exact_redeploy":
			return "exact_redeploy";
		case "DEPLOYMENT_ACTION_ROLLBACK":
		case "rollback":
			return "rollback";
		case "DEPLOYMENT_ACTION_CANCEL":
		case "cancel":
			return "cancel";
		case "DEPLOYMENT_ACTION_REMOVE":
		case "remove":
			return "remove";
		case "DEPLOYMENT_ACTION_RETRY":
		case "retry":
			return "retry";
		default:
			throw new Error(`invalid deployment action: ${String(raw)}`);
	}
}

export function decodeDeploymentState(raw: unknown): DashboardDeploymentState {
	switch (raw) {
		case "DEPLOYMENT_STATE_STAGED":
		case "staged":
			return "staged";
		case "DEPLOYMENT_STATE_QUEUED_BUILD":
		case "queued_build":
			return "queued_build";
		case "DEPLOYMENT_STATE_BUILDING":
		case "building":
			return "building";
		case "DEPLOYMENT_STATE_SCHEDULING":
		case "scheduling":
			return "scheduling";
		case "DEPLOYMENT_STATE_IMAGE_PULL":
		case "image_pull":
			return "image_pull";
		case "DEPLOYMENT_STATE_STARTING":
		case "starting":
			return "starting";
		case "DEPLOYMENT_STATE_READINESS":
		case "readiness":
			return "readiness";
		case "DEPLOYMENT_STATE_ACTIVE":
		case "active":
			return "active";
		case "DEPLOYMENT_STATE_DRAINING":
		case "draining":
			return "draining";
		case "DEPLOYMENT_STATE_COMPLETED":
		case "completed":
			return "completed";
		case "DEPLOYMENT_STATE_FAILED":
		case "failed":
			return "failed";
		case "DEPLOYMENT_STATE_CANCELLED":
		case "cancelled":
			return "cancelled";
		case "DEPLOYMENT_STATE_CRASHED":
		case "crashed":
			return "crashed";
		case "DEPLOYMENT_STATE_REMOVED":
		case "removed":
			return "removed";
		case "DEPLOYMENT_STATE_SUPERSEDED":
		case "superseded":
			return "superseded";
		default:
			return "unspecified";
	}
}

export function decodeDeploymentCauseKind(
	raw: unknown,
): DashboardDeploymentCauseKind {
	switch (raw) {
		case "DEPLOYMENT_CAUSE_KIND_USER":
		case "user":
			return "user";
		case "DEPLOYMENT_CAUSE_KIND_SYSTEM":
		case "system":
			return "system";
		case "DEPLOYMENT_CAUSE_KIND_AGENT":
		case "agent":
			return "agent";
		case "DEPLOYMENT_CAUSE_KIND_BUILDER":
		case "builder":
			return "builder";
		case "DEPLOYMENT_CAUSE_KIND_WEBHOOK":
		case "webhook":
			return "webhook";
		default:
			return "unspecified";
	}
}

export function decodeDomainOwnershipState(
	raw: unknown,
): DashboardDomainBinding["ownershipState"] {
	switch (raw) {
		case "DOMAIN_OWNERSHIP_STATE_VERIFIED":
		case "verified":
			return "verified";
		case "DOMAIN_OWNERSHIP_STATE_UNVERIFIED":
		case "unverified":
			return "unverified";
		default:
			return "unspecified";
	}
}

export function encodeServiceLogType(
	logType: DashboardServiceLogType | undefined,
): string | undefined {
	switch (logType) {
		case "runtime":
			return "SERVICE_LOG_TYPE_RUNTIME";
		case "build":
			return "SERVICE_LOG_TYPE_BUILD";
		case "deploy":
			return "SERVICE_LOG_TYPE_DEPLOY";
		case "http":
			return "SERVICE_LOG_TYPE_HTTP";
		case "network":
			return "SERVICE_LOG_TYPE_NETWORK";
		case "unspecified":
			return "SERVICE_LOG_TYPE_UNSPECIFIED";
		default:
			return undefined;
	}
}
