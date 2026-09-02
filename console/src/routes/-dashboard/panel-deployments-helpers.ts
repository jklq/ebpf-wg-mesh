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

import {
	deploymentCauseLabel,
	isInProgressDeploymentState,
} from "./deployment-inline";
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
			state: "succeeded",
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
		status?.state === "failed" ||
		status?.state === "crashed" ||
		status?.state === "cancelled"
	) {
		return "failed";
	}
	if (status?.state === "active") {
		return "active";
	}
	if (status?.state === "removed") {
		return "active";
	}
	if (status?.state === "draining" || status?.state === "completed") {
		return "draining";
	}
	if (
		status &&
		status.state !== "superseded" &&
		status.state !== "unspecified"
	) {
		return "running";
	}

	const failedStage = build?.stages?.find((stage) => stage.state === "failed");
	const failed =
		Boolean(failedStage) ||
		build?.state === "failed" ||
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

	if (!isCurrent && build?.state === "succeeded") {
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
	if (stage.state === "running" && stage.startedAt && nowMs) {
		return formatDuration(nowMs - stage.startedAt.getTime());
	}
	switch (stage.state) {
		case "running":
			return "In progress";
		case "pending":
			return "Not started";
		case "skipped":
			return "Skipped";
		case "failed":
			return "Failed";
		case "succeeded":
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
			case "completed":
			case "failed":
			case "cancelled":
			case "crashed":
			case "removed":
			case "superseded":
			case "active":
				return false;
			default:
				return true;
		}
	}
	return Boolean(build?.state === "queued" || build?.state === "running");
}

export function availableDeploymentActions(
	record: DashboardDeploymentRecord,
	deploymentInProgress: boolean,
): Array<DashboardDeploymentAction> {
	const state = record.status?.state;
	const image = record.imageDigest || record.status?.imageDigest || "";
	const hasImmutableImage = /@sha256:[0-9a-f]{64}$/i.test(image);
	const actions: Array<DashboardDeploymentAction> = [];
	if (record.isCurrent && state === "active") {
		actions.push("restart", "remove");
	}
	if (
		record.isCurrent &&
		state !== undefined &&
		isInProgressDeploymentState(state) &&
		state !== "active" &&
		state !== "draining"
	) {
		actions.push("cancel");
	}
	if (
		state === "failed" ||
		state === "cancelled" ||
		state === "crashed"
	) {
		actions.push("retry");
	}
	if (
		!record.isCurrent &&
		(state === "active" || state === "completed" || state === "draining") &&
		hasImmutableImage
	) {
		actions.push("rollback");
	}
	if (hasImmutableImage && !deploymentInProgress) {
		actions.push("exact_redeploy");
	}
	return actions;
}

export function actionLabel(action: DashboardDeploymentAction): string {
	switch (action) {
		case "restart":
			return "Restart";
		case "exact_redeploy":
			return "Redeploy";
		case "rollback":
			return "Rollback";
		case "cancel":
			return "Cancel";
		case "remove":
			return "Remove";
		case "retry":
			return "Retry";
	}
}

export function newIdempotencyKey(): string {
	if (typeof globalThis.crypto?.randomUUID === "function") {
		return globalThis.crypto.randomUUID();
	}
	return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

export function deploymentRecordKey(entry: DashboardDeploymentRecord): string {
	return entry.build?.buildId || `${entry.id}-${entry.rolloutGeneration}`;
}

export function compareDeploymentsNewestFirst(
	a: DashboardDeploymentRecord,
	b: DashboardDeploymentRecord,
): number {
	return deploymentTime(b) - deploymentTime(a);
}

export function mergeDeploymentRecords(
	records: Array<DashboardDeploymentRecord>,
): Array<DashboardDeploymentRecord> {
	const uniqueRecords = new Map<string, DashboardDeploymentRecord>();
	for (const record of records) {
		uniqueRecords.set(deploymentRecordKey(record), record);
	}
	return [...uniqueRecords.values()].sort(compareDeploymentsNewestFirst);
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

export function isSameDeploymentRecord(
	left: DashboardDeploymentRecord,
	right: DashboardDeploymentRecord,
): boolean {
	if (left.build?.buildId && right.build?.buildId) {
		return left.build.buildId === right.build.buildId;
	}
	return (
		left.rolloutGeneration !== undefined &&
		right.rolloutGeneration !== undefined &&
		left.rolloutGeneration === right.rolloutGeneration
	);
}

export function hasDeploymentIdentity(
	entry: DashboardDeploymentRecord,
): boolean {
	return Boolean(
		entry.build?.buildId !== undefined || entry.rolloutGeneration !== undefined,
	);
}

export function createDeploymentRecord({
	serviceId,
	build,
	allocation,
	rolloutGeneration,
	isCurrent,
	status,
}: {
	serviceId: string;
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	rolloutGeneration?: number;
	isCurrent: boolean;
	status?: DashboardDeploymentStatus;
}): DashboardDeploymentRecord {
	return {
		id: status?.deploymentId || serviceId,
		rolloutGeneration: status?.rolloutGeneration || rolloutGeneration || 0,
		createdAt:
			status?.transitionedAt ??
			build?.startedAt ??
			build?.queuedAt ??
			build?.finishedAt ??
			allocation?.updatedAt,
		build,
		allocation,
		isCurrent,
		status,
		imageDigest: status?.imageDigest,
	};
}

export function shouldRenderDeploymentHistoryEntry(
	entry: DashboardDeploymentRecord,
	service: DashboardServiceRecord,
): boolean {
	if (!usesRepositorySource(service)) {
		return true;
	}
	return Boolean(entry.build?.buildId);
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
	status: DashboardDeploymentStatus | undefined,
	timestamp: Date | undefined,
	nowMs: number,
): string[] {
	const parts: string[] = [];
	if (build?.commitSha) parts.push(shortSha(build.commitSha));
	if (build?.commitAuthor) parts.push(build.commitAuthor);
	if (timestamp) parts.push(formatRelativeAge(timestamp, nowMs));
	const cause = deploymentCauseLabel(status?.causeKind);
	if (cause) parts.push(cause);
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
