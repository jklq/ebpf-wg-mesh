import type {
	DashboardBuildStatus,
	DashboardDeploymentRecord,
	DashboardDeploymentStage,
	DashboardDeploymentState,
	DashboardDeploymentStatus,
} from "#/lib/dashboard/core/types.server";

const ENV_KEY = /[A-Z][A-Z0-9_]{1,63}/;
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

export function stageAttemptLabel(options: {
	state: string;
	detail?: string;
	priorFailed: boolean;
}): string | undefined {
	if (
		options.priorFailed &&
		(options.state === "pending" || options.state === "unspecified")
	) {
		return "not attempted";
	}
	if (options.detail?.trim()) {
		return options.detail.trim();
	}
	return undefined;
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

export function looksLikeEnvKey(value: string): boolean {
	return ENV_KEY.test(value) && isLikelyEnvKey(value);
}

const LIVE_STATES = new Set<DashboardDeploymentState>([
	"staged",
	"queued_build",
	"building",
	"scheduling",
	"image_pull",
	"starting",
	"readiness",
	"active",
	"draining",
]);

const IN_PROGRESS_STATES = new Set<DashboardDeploymentState>([
	"staged",
	"queued_build",
	"building",
	"scheduling",
	"image_pull",
	"starting",
	"readiness",
	"draining",
]);

const FAILED_STATES = new Set<DashboardDeploymentState>([
	"failed",
	"cancelled",
	"crashed",
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
	if (entry.status?.state === "removed") return false;
	if (entry.isCurrent) return true;
	if (isLiveDeploymentState(entry.status?.state)) return true;
	return Boolean(
		entry.build?.state === "queued" || entry.build?.state === "running",
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
		case "staged":
			return "Staged";
		case "queued_build":
			return "Queued";
		case "building":
			return "Building";
		case "scheduling":
			return "Scheduling";
		case "image_pull":
			return "Pulling";
		case "starting":
			return "Starting";
		case "readiness":
			return "Health";
		case "active":
			return "Active";
		case "draining":
			return "Draining";
		case "completed":
			return "Done";
		case "failed":
			return "Failed";
		case "cancelled":
			return "Cancelled";
		case "crashed":
			return "Crashed";
		case "removed":
			return "Removed";
		case "superseded":
			return "Superseded";
		default:
			if (build?.state === "queued") return "Queued";
			if (build?.state === "running") return "Building";
			if (build?.state === "failed") return "Failed";
			if (build?.state === "succeeded") return "Active";
			return "Deploy";
	}
}

export function deploymentCauseLabel(
	causeKind: DashboardDeploymentStatus["causeKind"] | undefined,
): string | undefined {
	switch (causeKind) {
		case "webhook":
			return "via GitHub";
		case "user":
			return "via dashboard";
		case "builder":
			return "via builder";
		case "agent":
			return "via agent";
		case "system":
			return "via system";
		default:
			return undefined;
	}
}

export function focusDeploymentStage(
	stages: Array<DashboardDeploymentStage>,
): DashboardDeploymentStage | undefined {
	return (
		stages.find((stage) => stage.state === "failed") ??
		stages.find((stage) => stage.state === "running") ??
		stages.find((stage) => stage.state === "pending")
	);
}

export function deploymentProgressCopy(options: {
	status?: DashboardDeploymentStatus;
	stages: Array<DashboardDeploymentStage>;
	build?: DashboardBuildStatus;
	stepHint?: string;
}): string {
	const state = options.status?.state;
	const focus = focusDeploymentStage(options.stages);
	if (state === "active" || state === "completed") {
		return "Deployment successful";
	}
	if (state === "draining") {
		return options.status?.detail?.trim() || "Draining traffic";
	}
	if (isFailedDeploymentState(state) || options.build?.state === "failed") {
		const failed = options.stages.find((stage) => stage.state === "failed");
		return (
			failed?.detail?.trim() ||
			options.build?.failureReason?.trim() ||
			options.status?.detail?.trim() ||
			"Deployment failed"
		);
	}
	if (
		isInProgressDeploymentState(state) ||
		focus?.state === "running" ||
		options.build?.state === "queued" ||
		options.build?.state === "running"
	) {
		const step =
			options.status?.detail?.trim() ||
			(focus?.state === "running" || focus?.state === "pending"
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
	if (options.build?.state === "succeeded") {
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
		case "staged":
			return "Configuration staged";
		case "queued_build":
			return "Build queued";
		case "building":
			return "Building image";
		case "scheduling":
			return "Scheduling rollout";
		case "image_pull":
			return "Pulling image";
		case "starting":
			return "Starting container";
		case "readiness":
			return "Waiting for readiness";
		default:
			if (build?.state === "queued") return "Build queued";
			if (build?.state === "running") return "Building image";
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
