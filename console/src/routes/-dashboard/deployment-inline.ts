import type {
	DashboardBuildStatus,
	DashboardDeploymentRecord,
	DashboardDeploymentStage,
	DashboardDeploymentState,
	DashboardDeploymentStatus,
} from "#/lib/dashboard/core/types.server";

const ENV_STOPWORDS = new Set([
	"ERROR",
	"FATAL",
	"WARN",
	"WARNING",
	"INFO",
	"DEBUG",
	"HTTP",
	"HTTPS",
	"JSON",
	"TRUE",
	"FALSE",
	"NULL",
	"EXIT",
	"CODE",
	"RUN",
	"FROM",
	"COPY",
	"WORKDIR",
	"BUILD",
	"STEP",
]);

const ERROR_LINE =
	/\b(fatal|error|failed|required|exit code|does not exist|not set|undefined)\b/i;

const ENV_PATTERNS: RegExp[] = [
	/\b([A-Z][A-Z0-9_]{1,63})\s+is required\b/g,
	/\brequired(?:\s+at\s+(?:build|runtime)\s+time)?[:\s]+([A-Z][A-Z0-9_]{1,63})\b/gi,
	/\bmissing(?:\s+env(?:ironment)?(?:\s+variable)?)?[:\s]+([A-Z][A-Z0-9_]{1,63})\b/gi,
	/\bundefined(?:\s+environment)?\s+variable[:\s]+([A-Z][A-Z0-9_]{1,63})\b/gi,
	/\$\{([A-Z][A-Z0-9_]{1,63})\}\s+is\s+(?:not set|required|empty|undefined)\b/g,
];

export function extractMissingEnvKeys(
	texts: Array<string | undefined>,
): string[] {
	const found = new Set<string>();
	for (const text of texts) {
		if (!text) continue;
		for (const pattern of ENV_PATTERNS) {
			pattern.lastIndex = 0;
			for (const match of text.matchAll(pattern)) {
				const key = match[1];
				if (key && isLikelyEnvKey(key)) {
					found.add(key);
				}
			}
		}
	}
	return [...found];
}

export function isErrorLogLine(line: string): boolean {
	return ERROR_LINE.test(line);
}

export function selectInlineLogSnippet(
	lines: string[],
	options?: { max?: number },
): { lines: string[]; highlightIndexes: number[] } {
	const max = options?.max ?? 5;
	if (lines.length === 0) {
		return { lines: [], highlightIndexes: [] };
	}

	const errorIndex = findLastIndex(lines, isErrorLogLine);
	const window =
		errorIndex >= 0 ? sliceAround(lines, errorIndex, max) : lines.slice(-max);
	const highlightIndexes = window.flatMap((line, index) =>
		isErrorLogLine(line) ? [index] : [],
	);
	return { lines: window, highlightIndexes };
}

export function buildStepHint(lines: string[]): string | undefined {
	let fallback: string | undefined;
	for (let index = lines.length - 1; index >= 0; index -= 1) {
		const stepOf = lines[index].match(/\bstep\s+(\d+)\s+of\s+(\d+)\b/i);
		if (stepOf) {
			return `step ${stepOf[1]} of ${stepOf[2]}`;
		}
		const hashStep = lines[index].match(/^#(\d+)\b/);
		if (!hashStep) continue;
		const total = lines[index].match(/\[(?:[^\]]+?)\s+(\d+)\/(\d+)\]/);
		if (total) {
			return `step ${hashStep[1]} of ${total[2]}`;
		}
		fallback ??= `step ${hashStep[1]}`;
	}
	return fallback;
}

export function trafficRetentionCopy(options: {
	failed: boolean;
	lastSuccessfulCommitSha?: string;
}): string | undefined {
	if (!options.failed) return undefined;
	if (options.lastSuccessfulCommitSha) {
		return "the last healthy rollout. Nothing was taken down.";
	}
	return "This first rollout never left the builder. Nothing is serving yet.";
}

const LIVE_STATES = new Set<DashboardDeploymentState>([
	"DEPLOYMENT_STATE_STAGED",
	"DEPLOYMENT_STATE_QUEUED_BUILD",
	"DEPLOYMENT_STATE_BUILDING",
	"DEPLOYMENT_STATE_SCHEDULING",
	"DEPLOYMENT_STATE_IMAGE_PULL",
	"DEPLOYMENT_STATE_STARTING",
	"DEPLOYMENT_STATE_READINESS",
	"DEPLOYMENT_STATE_ACTIVE",
	"DEPLOYMENT_STATE_DRAINING",
]);

const IN_PROGRESS_STATES = new Set<DashboardDeploymentState>([
	"DEPLOYMENT_STATE_STAGED",
	"DEPLOYMENT_STATE_QUEUED_BUILD",
	"DEPLOYMENT_STATE_BUILDING",
	"DEPLOYMENT_STATE_SCHEDULING",
	"DEPLOYMENT_STATE_IMAGE_PULL",
	"DEPLOYMENT_STATE_STARTING",
	"DEPLOYMENT_STATE_READINESS",
	"DEPLOYMENT_STATE_DRAINING",
]);

const FAILED_STATES = new Set<DashboardDeploymentState>([
	"DEPLOYMENT_STATE_FAILED",
	"DEPLOYMENT_STATE_CANCELLED",
	"DEPLOYMENT_STATE_CRASHED",
]);

export function isLiveDeploymentState(
	state: DashboardDeploymentState | undefined,
): boolean {
	return Boolean(state && LIVE_STATES.has(state));
}

export function isInProgressDeploymentState(
	state: DashboardDeploymentState | undefined,
): boolean {
	return Boolean(state && IN_PROGRESS_STATES.has(state));
}

export function isFailedDeploymentState(
	state: DashboardDeploymentState | undefined,
): boolean {
	return Boolean(state && FAILED_STATES.has(state));
}

export function isPinnedDeployment(
	entry: Pick<DashboardDeploymentRecord, "isCurrent" | "status" | "build">,
): boolean {
	if (entry.status?.state === "DEPLOYMENT_STATE_REMOVED") return false;
	if (entry.isCurrent) return true;
	if (isLiveDeploymentState(entry.status?.state)) return true;
	return Boolean(
		entry.build?.state === "BUILD_STATE_QUEUED" ||
			entry.build?.state === "BUILD_STATE_RUNNING",
	);
}

export function partitionDeployments<T extends DashboardDeploymentRecord>(
	records: T[],
): { live: T[]; history: T[] } {
	const live: T[] = [];
	const history: T[] = [];
	for (const record of records) {
		if (isPinnedDeployment(record)) {
			live.push(record);
		} else {
			history.push(record);
		}
	}
	return { live, history };
}

export function deploymentBadgeLabel(
	state: DashboardDeploymentState | undefined,
	build?: DashboardBuildStatus,
): string {
	switch (state) {
		case "DEPLOYMENT_STATE_STAGED":
			return "Staged";
		case "DEPLOYMENT_STATE_QUEUED_BUILD":
			return "Queued";
		case "DEPLOYMENT_STATE_BUILDING":
			return "Building";
		case "DEPLOYMENT_STATE_SCHEDULING":
			return "Scheduling";
		case "DEPLOYMENT_STATE_IMAGE_PULL":
			return "Pulling";
		case "DEPLOYMENT_STATE_STARTING":
			return "Starting";
		case "DEPLOYMENT_STATE_READINESS":
			return "Health";
		case "DEPLOYMENT_STATE_ACTIVE":
			return "Active";
		case "DEPLOYMENT_STATE_DRAINING":
			return "Draining";
		case "DEPLOYMENT_STATE_COMPLETED":
			return "Done";
		case "DEPLOYMENT_STATE_FAILED":
			return "Failed";
		case "DEPLOYMENT_STATE_CANCELLED":
			return "Cancelled";
		case "DEPLOYMENT_STATE_CRASHED":
			return "Crashed";
		case "DEPLOYMENT_STATE_REMOVED":
			return "Removed";
		case "DEPLOYMENT_STATE_SUPERSEDED":
			return "Superseded";
		default:
			if (build?.state === "BUILD_STATE_QUEUED") return "Queued";
			if (build?.state === "BUILD_STATE_RUNNING") return "Building";
			if (build?.state === "BUILD_STATE_FAILED") return "Failed";
			if (build?.state === "BUILD_STATE_SUCCEEDED") return "Active";
			return "Deploy";
	}
}

export function focusDeploymentStage(
	stages: Array<DashboardDeploymentStage>,
): DashboardDeploymentStage | undefined {
	return (
		stages.find((stage) => stage.state === "DEPLOYMENT_STAGE_STATE_FAILED") ??
		stages.find((stage) => stage.state === "DEPLOYMENT_STAGE_STATE_RUNNING") ??
		stages.find((stage) => stage.state === "DEPLOYMENT_STAGE_STATE_PENDING")
	);
}

export function deploymentStagesForDisplay(
	status: DashboardDeploymentStatus | undefined,
	build: DashboardBuildStatus | undefined,
	reported: Array<DashboardDeploymentStage>,
): Array<DashboardDeploymentStage> {
	if (reported.length > 0) return reported;
	const state = status?.state;
	if (!state && !build) return [];

	const buildRunning =
		state === "DEPLOYMENT_STATE_BUILDING" ||
		build?.state === "BUILD_STATE_RUNNING";
	const buildFailed =
		build?.state === "BUILD_STATE_FAILED" ||
		state === "DEPLOYMENT_STATE_FAILED";
	const buildComplete = Boolean(
		build?.state === "BUILD_STATE_SUCCEEDED" ||
			state === "DEPLOYMENT_STATE_SCHEDULING" ||
			state === "DEPLOYMENT_STATE_IMAGE_PULL" ||
			state === "DEPLOYMENT_STATE_STARTING" ||
			state === "DEPLOYMENT_STATE_READINESS" ||
			state === "DEPLOYMENT_STATE_ACTIVE" ||
			state === "DEPLOYMENT_STATE_DRAINING" ||
			state === "DEPLOYMENT_STATE_COMPLETED",
	);
	const deployRunning = Boolean(
		state === "DEPLOYMENT_STATE_SCHEDULING" ||
			state === "DEPLOYMENT_STATE_IMAGE_PULL" ||
			state === "DEPLOYMENT_STATE_STARTING",
	);
	const deployComplete = Boolean(
		state === "DEPLOYMENT_STATE_READINESS" ||
			state === "DEPLOYMENT_STATE_ACTIVE" ||
			state === "DEPLOYMENT_STATE_DRAINING" ||
			state === "DEPLOYMENT_STATE_COMPLETED",
	);
	const postDeployRunning = state === "DEPLOYMENT_STATE_READINESS";
	const postDeployComplete = Boolean(
		state === "DEPLOYMENT_STATE_ACTIVE" ||
			state === "DEPLOYMENT_STATE_DRAINING" ||
			state === "DEPLOYMENT_STATE_COMPLETED",
	);

	return [
		{
			key: "build",
			label: "Build",
			detail: buildFailed
				? build?.failureReason || status?.detail || "Build failed"
				: buildRunning
					? status?.detail || "Building image"
					: buildComplete
						? "Image ready"
						: "Waiting for build",
			state: buildFailed
				? "DEPLOYMENT_STAGE_STATE_FAILED"
				: buildRunning
					? "DEPLOYMENT_STAGE_STATE_RUNNING"
					: buildComplete
						? "DEPLOYMENT_STAGE_STATE_SUCCEEDED"
						: "DEPLOYMENT_STAGE_STATE_PENDING",
		},
		{
			key: "deploy",
			label: "Deploy",
			detail: deployRunning
				? status?.detail || "Deploying service"
				: deployComplete
					? "Service running"
					: "Waiting for build to finish",
			state: deployRunning
				? "DEPLOYMENT_STAGE_STATE_RUNNING"
				: deployComplete
					? "DEPLOYMENT_STAGE_STATE_SUCCEEDED"
					: "DEPLOYMENT_STAGE_STATE_PENDING",
		},
		{
			key: "post-deploy",
			label: "Post-deploy",
			detail: postDeployRunning
				? status?.detail || "Waiting for readiness"
				: postDeployComplete
					? "Deployment ready"
					: "Waiting for rollout",
			state: postDeployRunning
				? "DEPLOYMENT_STAGE_STATE_RUNNING"
				: postDeployComplete
					? "DEPLOYMENT_STAGE_STATE_SUCCEEDED"
					: "DEPLOYMENT_STAGE_STATE_PENDING",
		},
	];
}

export function deploymentProgressCopy(options: {
	status?: DashboardDeploymentStatus;
	stages: Array<DashboardDeploymentStage>;
	build?: DashboardBuildStatus;
	stepHint?: string;
}): string {
	const state = options.status?.state;
	const focus = focusDeploymentStage(options.stages);
	if (
		state === "DEPLOYMENT_STATE_ACTIVE" ||
		state === "DEPLOYMENT_STATE_COMPLETED"
	) {
		return "Deployment successful";
	}
	if (state === "DEPLOYMENT_STATE_DRAINING") {
		return options.status?.detail?.trim() || "Draining traffic";
	}
	if (
		isFailedDeploymentState(state) ||
		options.build?.state === "BUILD_STATE_FAILED"
	) {
		const failed = options.stages.find(
			(stage) => stage.state === "DEPLOYMENT_STAGE_STATE_FAILED",
		);
		return (
			failed?.detail?.trim() ||
			options.build?.failureReason?.trim() ||
			options.status?.detail?.trim() ||
			"Deployment failed"
		);
	}
	if (
		isInProgressDeploymentState(state) ||
		focus?.state === "DEPLOYMENT_STAGE_STATE_RUNNING" ||
		options.build?.state === "BUILD_STATE_QUEUED" ||
		options.build?.state === "BUILD_STATE_RUNNING"
	) {
		const step =
			options.status?.detail?.trim() ||
			(focus?.state === "DEPLOYMENT_STAGE_STATE_RUNNING" ||
			focus?.state === "DEPLOYMENT_STAGE_STATE_PENDING"
				? focus.detail?.trim() || focus.label?.trim()
				: undefined) ||
			defaultProgressForState(state, options.build);
		const hint =
			options.stepHint && focus?.key === "build" ? options.stepHint : undefined;
		if (hint && step) {
			return `Deployment in progress: ${step} · ${hint}`;
		}
		if (hint) {
			return `Deployment in progress: ${hint}`;
		}
		return step ? `Deployment in progress: ${step}` : "Deployment in progress";
	}
	if (options.build?.state === "BUILD_STATE_SUCCEEDED") {
		return "Deployment successful";
	}
	if (!state && !options.build) {
		return "No deployment has started yet";
	}
	return options.status?.detail?.trim() || "Deployment successful";
}

function defaultProgressForState(
	state: DashboardDeploymentState | undefined,
	build?: DashboardBuildStatus,
): string | undefined {
	switch (state) {
		case "DEPLOYMENT_STATE_STAGED":
			return "Configuration staged";
		case "DEPLOYMENT_STATE_QUEUED_BUILD":
			return "Build queued";
		case "DEPLOYMENT_STATE_BUILDING":
			return "Building image";
		case "DEPLOYMENT_STATE_SCHEDULING":
			return "Scheduling rollout";
		case "DEPLOYMENT_STATE_IMAGE_PULL":
			return "Pulling image";
		case "DEPLOYMENT_STATE_STARTING":
			return "Starting container";
		case "DEPLOYMENT_STATE_READINESS":
			return "Waiting for readiness";
		default:
			if (build?.state === "BUILD_STATE_QUEUED") return "Build queued";
			if (build?.state === "BUILD_STATE_RUNNING") return "Building image";
			return undefined;
	}
}

function isLikelyEnvKey(key: string): boolean {
	if (ENV_STOPWORDS.has(key)) return false;
	return key.includes("_") || key.length >= 6;
}

function sliceAround(lines: string[], center: number, max: number): string[] {
	const before = Math.max(0, Math.ceil((max - 1) / 2) - 1);
	const start = Math.max(0, center - before);
	const end = Math.min(lines.length, start + max);
	const adjustedStart = Math.max(0, end - max);
	return lines.slice(adjustedStart, end);
}

function findLastIndex(
	items: string[],
	predicate: (item: string) => boolean,
): number {
	for (let index = items.length - 1; index >= 0; index -= 1) {
		if (predicate(items[index])) return index;
	}
	return -1;
}
