import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDeploymentStage,
	DashboardDeploymentStatus,
	DashboardServiceLogLine,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { isInProgressDeploymentState } from "./deployment-inline";
import { shortId, shortSha } from "./service-utils";

export type LogTypeFilter = "all" | "deploy" | "build" | "runtime";
export type DeploymentCardTone = "running" | "active" | "draining" | "failed";

export function logLinesForStage(
	lines: Array<DashboardServiceLogLine>,
	stageKey: string | undefined,
): Array<DashboardServiceLogLine> {
	const matching = stageKey
		? lines.filter((line) => !line.stage || line.stage === stageKey)
		: lines;
	const source = matching.length > 0 ? matching : lines;
	return source.filter((line) => line.line.trim() !== "");
}

export function withSourceStage(
	service: DashboardServiceRecord,
	build: DashboardBuildStatus | undefined,
	stages: Array<DashboardDeploymentStage>,
): Array<DashboardDeploymentStage> {
	if (
		stages.some(
			(stage) => stage.key === "initialization" || stage.key === "source",
		)
	) {
		return stages;
	}
	const source = service.spec?.source;
	if (!source?.repositorySelector) {
		return stages;
	}
	const sha = build?.commitSha ? shortSha(build.commitSha) : undefined;
	const repo =
		source.repositorySelector.split("/").slice(-2).join("/") ||
		source.repositorySelector;
	return [
		{
			key: "source",
			label: "Source",
			detail: sha ? `${repo} @ ${sha}` : repo,
			state: "DEPLOYMENT_STAGE_STATE_SUCCEEDED",
			startedAt: build?.queuedAt ?? build?.startedAt,
			finishedAt: build?.startedAt ?? build?.queuedAt,
		},
		...stages,
	];
}

export function getDeploymentCardTone({
	build,
	allocation,
	active,
	isCurrent,
	status,
}: {
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	active: boolean;
	isCurrent: boolean;
	status?: DashboardDeploymentStatus;
}): DeploymentCardTone | undefined {
	if (
		status?.state === "DEPLOYMENT_STATE_FAILED" ||
		status?.state === "DEPLOYMENT_STATE_CRASHED" ||
		status?.state === "DEPLOYMENT_STATE_CANCELLED"
	) {
		return "failed";
	}
	if (status?.state === "DEPLOYMENT_STATE_ACTIVE") {
		return "active";
	}
	if (status?.state === "DEPLOYMENT_STATE_REMOVED") {
		return "active";
	}
	if (
		status?.state === "DEPLOYMENT_STATE_DRAINING" ||
		status?.state === "DEPLOYMENT_STATE_COMPLETED"
	) {
		return "draining";
	}
	if (
		status &&
		status.state !== "DEPLOYMENT_STATE_SUPERSEDED" &&
		status.state !== "DEPLOYMENT_STATE_UNSPECIFIED"
	) {
		return "running";
	}

	const failedStage = build?.stages?.find(
		(stage) => stage.state === "DEPLOYMENT_STAGE_STATE_FAILED",
	);
	const failed =
		Boolean(failedStage) ||
		build?.state === "BUILD_STATE_FAILED" ||
		Boolean(allocation?.message && !allocation.healthy && !active);

	if (failed) {
		return "failed";
	}

	if (active) {
		return "running";
	}

	if (isCurrent && !active && !failed && allocation?.healthy) {
		return "active";
	}

	if (!isCurrent && build?.state === "BUILD_STATE_SUCCEEDED") {
		return "draining";
	}

	return undefined;
}

export function stageStatusText(
	stage: DashboardDeploymentStage,
	nowMs?: number,
): string {
	if (stage.startedAt && stage.finishedAt) {
		return formatDuration(
			stage.finishedAt.getTime() - stage.startedAt.getTime(),
		);
	}
	if (
		stage.state === "DEPLOYMENT_STAGE_STATE_RUNNING" &&
		stage.startedAt &&
		nowMs
	) {
		return formatDuration(nowMs - stage.startedAt.getTime());
	}
	switch (stage.state) {
		case "DEPLOYMENT_STAGE_STATE_RUNNING":
			return "In progress";
		case "DEPLOYMENT_STAGE_STATE_PENDING":
			return "Not started";
		case "DEPLOYMENT_STAGE_STATE_SKIPPED":
			return "Skipped";
		case "DEPLOYMENT_STAGE_STATE_FAILED":
			return "Failed";
		case "DEPLOYMENT_STAGE_STATE_SUCCEEDED":
			return "Done";
		default:
			return "Waiting";
	}
}

export function hasActiveDeployment(
	status: DashboardDeploymentStatus | undefined,
	build: DashboardBuildStatus | undefined,
): boolean {
	if (status) {
		switch (status.state) {
			case "DEPLOYMENT_STATE_COMPLETED":
			case "DEPLOYMENT_STATE_FAILED":
			case "DEPLOYMENT_STATE_CANCELLED":
			case "DEPLOYMENT_STATE_CRASHED":
			case "DEPLOYMENT_STATE_REMOVED":
			case "DEPLOYMENT_STATE_SUPERSEDED":
			case "DEPLOYMENT_STATE_ACTIVE":
				return false;
			default:
				return true;
		}
	}
	return Boolean(
		build?.state === "BUILD_STATE_QUEUED" ||
			build?.state === "BUILD_STATE_RUNNING",
	);
}

export function availableDeploymentActions(
	record: DashboardDeploymentRecord,
	deploymentInProgress: boolean,
): Array<DashboardDeploymentAction> {
	const state = record.status?.state;
	const image = record.imageDigest || record.status?.imageDigest || "";
	const hasImmutableImage = /@sha256:[0-9a-f]{64}$/i.test(image);
	const actions: Array<DashboardDeploymentAction> = [];
	if (record.isCurrent && state === "DEPLOYMENT_STATE_ACTIVE") {
		actions.push("DEPLOYMENT_ACTION_RESTART", "DEPLOYMENT_ACTION_REMOVE");
	}
	if (
		record.isCurrent &&
		state !== undefined &&
		isInProgressDeploymentState(state) &&
		state !== "DEPLOYMENT_STATE_ACTIVE" &&
		state !== "DEPLOYMENT_STATE_DRAINING"
	) {
		actions.push("DEPLOYMENT_ACTION_CANCEL");
	}
	if (
		state === "DEPLOYMENT_STATE_FAILED" ||
		state === "DEPLOYMENT_STATE_CANCELLED" ||
		state === "DEPLOYMENT_STATE_CRASHED"
	) {
		actions.push("DEPLOYMENT_ACTION_RETRY");
	}
	if (
		!record.isCurrent &&
		(state === "DEPLOYMENT_STATE_ACTIVE" ||
			state === "DEPLOYMENT_STATE_COMPLETED" ||
			state === "DEPLOYMENT_STATE_DRAINING" ||
			state === "DEPLOYMENT_STATE_REMOVED") &&
		hasImmutableImage
	) {
		actions.push("DEPLOYMENT_ACTION_ROLLBACK");
	}
	if (hasImmutableImage && !deploymentInProgress) {
		actions.push("DEPLOYMENT_ACTION_EXACT_REDEPLOY");
	}
	return actions;
}

export function actionLabel(action: DashboardDeploymentAction): string {
	switch (action) {
		case "DEPLOYMENT_ACTION_RESTART":
			return "Restart";
		case "DEPLOYMENT_ACTION_EXACT_REDEPLOY":
			return "Redeploy";
		case "DEPLOYMENT_ACTION_ROLLBACK":
			return "Rollback";
		case "DEPLOYMENT_ACTION_CANCEL":
			return "Cancel";
		case "DEPLOYMENT_ACTION_REMOVE":
			return "Remove";
		case "DEPLOYMENT_ACTION_RETRY":
			return "Retry";
	}
}

export function newIdempotencyKey(): string {
	if (typeof globalThis.crypto?.randomUUID === "function") {
		return globalThis.crypto.randomUUID();
	}
	return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

export function compareDeploymentsNewestFirst(
	a: DashboardDeploymentRecord,
	b: DashboardDeploymentRecord,
): number {
	return deploymentTime(b) - deploymentTime(a);
}

export function reconcileCurrentDeployment(
	records: Array<DashboardDeploymentRecord>,
	service: DashboardServiceRecord,
): Array<DashboardDeploymentRecord> {
	const status = service.latestDeployment;
	const build = service.latestBuild;
	if (!status && !build) return records;

	const rolloutGeneration =
		status?.rolloutGeneration ?? service.rolloutGeneration ?? 0;
	if (rolloutGeneration === 0 && status?.state === "DEPLOYMENT_STATE_STAGED") {
		return records;
	}
	const deploymentId = status?.deploymentId;
	const matchIndex = records.findIndex(
		(record) =>
			(Boolean(deploymentId) && record.id === deploymentId) ||
			(Boolean(build?.buildId) && record.build?.buildId === build?.buildId) ||
			(record.rolloutGeneration === rolloutGeneration && rolloutGeneration > 0),
	);
	const current = matchIndex >= 0 ? records[matchIndex] : undefined;
	const mergedBuild = build
		? {
				...current?.build,
				...build,
				stages:
					(build.stages?.length ?? 0) > 0
						? build.stages
						: current?.build?.stages,
			}
		: current?.build;
	const next: DashboardDeploymentRecord = {
		...current,
		id:
			deploymentId ||
			current?.id ||
			build?.buildId ||
			`${service.id}:${rolloutGeneration}`,
		rolloutGeneration,
		specRevision: status?.specRevision ?? service.specRevision,
		createdAt:
			current?.createdAt ??
			status?.transitionedAt ??
			build?.queuedAt ??
			build?.startedAt ??
			service.updatedAt,
		repositorySelector: service.spec?.source?.repositorySelector,
		trackedRef: service.spec?.source?.trackedRef,
		build: mergedBuild,
		isCurrent: true,
		status: status ?? current?.status,
		stages: (build?.stages?.length ?? 0) > 0 ? build?.stages : current?.stages,
		imageDigest:
			build?.imageDigest || status?.imageDigest || current?.imageDigest,
	};

	if (matchIndex < 0) return [next, ...records];
	return records.map((record, index) => (index === matchIndex ? next : record));
}

export function deploymentTime(entry: DashboardDeploymentRecord): number {
	return (
		entry.createdAt?.getTime() ??
		entry.build?.startedAt?.getTime() ??
		entry.build?.queuedAt?.getTime() ??
		entry.build?.finishedAt?.getTime() ??
		entry.allocation?.updatedAt?.getTime() ??
		0
	);
}

export function shouldRenderDeploymentHistoryEntry(
	entry: DashboardDeploymentRecord,
	service: DashboardServiceRecord,
): boolean {
	if (
		entry.rolloutGeneration === 0 &&
		entry.status?.state === "DEPLOYMENT_STATE_STAGED"
	) {
		return false;
	}
	if (entry.isCurrent || isInProgressDeploymentState(entry.status?.state)) {
		return true;
	}
	if (!usesRepositorySource(service)) {
		return true;
	}
	return Boolean(entry.build?.buildId);
}

export type SourceRevisionState = "deployed" | "waiting" | "ignored";

export function sourceRevisionState(
	service: DashboardServiceRecord,
	autoDeploy: boolean,
): { state: SourceRevisionState; commitSha: string } | undefined {
	const latest = service.sourceSummary?.latestRevision?.commitSha;
	if (!latest) {
		return undefined;
	}
	if (latest === service.lastSuccessfulCommitSha) {
		return { state: "deployed", commitSha: latest };
	}
	const build = service.latestBuild;
	if (
		build?.commitSha === latest &&
		(build.state === "BUILD_STATE_FAILED" ||
			build.state === "BUILD_STATE_CANCELLED")
	) {
		return undefined;
	}
	const building =
		build?.commitSha === latest &&
		(build.state === "BUILD_STATE_QUEUED" ||
			build.state === "BUILD_STATE_RUNNING");
	if (autoDeploy || building) {
		return { state: "waiting", commitSha: latest };
	}
	return { state: "ignored", commitSha: latest };
}

export function usesRepositorySource(service: DashboardServiceRecord): boolean {
	return Boolean(
		service.spec?.source?.provider || service.sourceSummary?.desiredSpec,
	);
}

export function deploymentTitle(
	build: DashboardBuildStatus | undefined,
	isCurrent: boolean,
): string {
	if (build?.commitMessage?.trim()) return build.commitMessage.trim();
	if (build?.commitSha) return `Commit ${shortSha(build.commitSha)}`;
	if (build?.buildId) return `Build ${shortId(build.buildId)}`;
	return isCurrent ? "Current deployment" : "Previous deployment";
}

export function deploymentCardHeadline(
	build: DashboardBuildStatus | undefined,
): string {
	if (build?.commitMessage?.trim()) return build.commitMessage.trim();
	if (build?.commitSha) return `Commit ${shortSha(build.commitSha)}`;
	if (build?.buildId) return `Build ${shortId(build.buildId)}`;
	return "No commit message";
}

export function deploymentSubtitle(
	build: DashboardBuildStatus | undefined,
	rolloutGeneration: number | undefined,
	nowMs: number,
): string | undefined {
	const timestamp =
		build?.startedAt ?? build?.queuedAt ?? build?.finishedAt ?? undefined;
	if (timestamp) return formatRelativeAge(timestamp, nowMs);
	if (rolloutGeneration !== undefined) return `Rollout ${rolloutGeneration}`;
	return undefined;
}

export function matchesDeploymentLog(
	line: DashboardServiceLogLine,
	scope: {
		buildId?: string;
		allocationId?: string;
		rolloutGeneration?: number;
	},
): boolean {
	if (!scope.buildId && !scope.allocationId && !scope.rolloutGeneration) {
		return true;
	}
	if (scope.buildId && line.buildId === scope.buildId) return true;
	if (scope.allocationId && line.allocationId === scope.allocationId) {
		return true;
	}
	if (
		scope.rolloutGeneration !== undefined &&
		line.rolloutGeneration === scope.rolloutGeneration
	) {
		return true;
	}
	return false;
}

export function formatDuration(ms: number): string {
	const seconds = Math.max(0, Math.round(ms / 1000));
	if (seconds < 60) return `${seconds}s`;
	return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

export function deploymentMeta(
	build: DashboardBuildStatus | undefined,
	timestamp: Date | undefined,
	nowMs: number,
): string[] {
	const parts: string[] = [];
	if (build?.commitSha) parts.push(shortSha(build.commitSha));
	if (build?.commitAuthor) parts.push(build.commitAuthor);
	if (timestamp) parts.push(formatRelativeAge(timestamp, nowMs));
	return parts;
}

export function toneToHealthClass(
	tone: DeploymentCardTone,
): "building" | "healthy" | "failed" | "offline" {
	switch (tone) {
		case "running":
			return "building";
		case "active":
			return "healthy";
		case "draining":
			return "offline";
		case "failed":
			return "failed";
	}
}

export function formatLogTime(date: Date): string {
	return new Intl.DateTimeFormat(undefined, {
		hour: "2-digit",
		minute: "2-digit",
		second: "2-digit",
		hour12: false,
	}).format(date);
}

export function formatRelativeAge(date: Date, nowMs: number): string {
	const diffMs = date.getTime() - nowMs;
	const absSeconds = Math.round(Math.abs(diffMs) / 1000);
	const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

	if (absSeconds < 60) {
		return rtf.format(Math.round(diffMs / 1000), "second");
	}

	const absMinutes = Math.round(absSeconds / 60);
	if (absMinutes < 60) {
		return rtf.format(Math.round(diffMs / 60_000), "minute");
	}

	const absHours = Math.round(absMinutes / 60);
	if (absHours < 24) {
		return rtf.format(Math.round(diffMs / 3_600_000), "hour");
	}

	return rtf.format(Math.round(diffMs / 86_400_000), "day");
}

export function formatError(error: unknown, fallback: string): string {
	if (error && typeof error === "object" && "message" in error) {
		return String((error as { message: unknown }).message);
	}
	return fallback;
}

export function hydrateDeploymentRecord(
	record: DashboardDeploymentRecord,
): DashboardDeploymentRecord {
	return {
		...record,
		createdAt: hydrateDate(record.createdAt),
		build: hydrateBuildStatus(record.build),
		allocation: hydrateAllocationStatus(record.allocation),
		status: record.status
			? {
					...record.status,
					transitionedAt: hydrateDate(record.status.transitionedAt),
				}
			: undefined,
		stages:
			record.stages?.map((stage) => ({
				...stage,
				startedAt: hydrateDate(stage.startedAt),
				finishedAt: hydrateDate(stage.finishedAt),
			})) ?? record.stages,
		actions:
			record.actions?.map((action) => ({
				...action,
				createdAt: hydrateDate(action.createdAt),
			})) ?? record.actions,
	};
}

export function hydrateBuildStatus(
	build: DashboardBuildStatus | undefined,
): DashboardBuildStatus | undefined {
	if (!build) return undefined;
	return {
		...build,
		queuedAt: hydrateDate(build.queuedAt),
		startedAt: hydrateDate(build.startedAt),
		finishedAt: hydrateDate(build.finishedAt),
		stages:
			build.stages?.map((stage) => ({
				...stage,
				startedAt: hydrateDate(stage.startedAt),
				finishedAt: hydrateDate(stage.finishedAt),
			})) ?? [],
	};
}

export function hydrateAllocationStatus(
	allocation: DashboardAllocationStatus | undefined,
): DashboardAllocationStatus | undefined {
	if (!allocation) return undefined;
	return {
		...allocation,
		updatedAt: hydrateDate(allocation.updatedAt),
	};
}

export function hydrateServiceLogLine(
	line: DashboardServiceLogLine,
): DashboardServiceLogLine {
	return {
		...line,
		observedAt: hydrateDate(line.observedAt),
	};
}

export function hydrateDate(
	value: Date | string | undefined,
): Date | undefined {
	if (!value) return undefined;
	return value instanceof Date ? value : new Date(value);
}
