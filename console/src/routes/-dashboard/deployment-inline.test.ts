import { describe, expect, it } from "vitest";

import {
	buildStepHint,
	deploymentBadgeLabel,
	deploymentProgressCopy,
	extractMissingEnvKeys,
	isPinnedDeployment,
	partitionDeployments,
	selectInlineLogSnippet,
	trafficRetentionCopy,
} from "./deployment-inline";

describe("extractMissingEnvKeys", () => {
	it("reads a required-at-build-time env key", () => {
		expect(
			extractMissingEnvKeys([
				"#6 0.418 fatal: STRIPE_KEY is required at build time",
			]),
		).toEqual(["STRIPE_KEY"]);
	});

	it("ignores log noise that looks like uppercase words", () => {
		expect(
			extractMissingEnvKeys([
				"#6 ERROR: process did not complete successfully",
			]),
		).toEqual([]);
	});
});

describe("selectInlineLogSnippet", () => {
	it("keeps the failing line in view with surrounding context", () => {
		const snippet = selectInlineLogSnippet(
			[
				"#6 [builder 5/7] RUN go build -o /out/worker ./cmd/worker",
				"#6 0.412 config: reading environment",
				"#6 0.418 fatal: STRIPE_KEY is required at build time",
				"#6 ERROR: process did not complete successfully: exit code 1",
			],
			{ max: 4 },
		);
		expect(snippet.lines).toHaveLength(4);
		expect(snippet.highlightIndexes.length).toBeGreaterThan(0);
		expect(snippet.lines[snippet.highlightIndexes[0]]).toContain("STRIPE_KEY");
	});
});

describe("buildStepHint", () => {
	it("reads a BuildKit step number", () => {
		expect(
			buildStepHint([
				"#6 [builder 5/7] RUN go build -o /out/worker ./cmd/worker",
			]),
		).toBe("step 6 of 7");
	});
});

describe("trafficRetentionCopy", () => {
	it("explains that a first failed rollout served nothing", () => {
		expect(trafficRetentionCopy({ failed: true })).toBe(
			"This first rollout never left the builder. Nothing is serving yet.",
		);
	});

	it("keeps the last healthy sha in place on a later failure", () => {
		expect(
			trafficRetentionCopy({
				failed: true,
				lastSuccessfulCommitSha: "5b1c0afabc",
			}),
		).toBe("the last healthy rollout. Nothing was taken down.");
	});
});

describe("partitionDeployments", () => {
	it("moves the current deployment to history once it is removed", () => {
		const removed = {
			id: "deploy-1",
			rolloutGeneration: 1,
			isCurrent: true,
			status: {
				deploymentId: "deploy-1",
				state: "DEPLOYMENT_STATE_REMOVED" as const,
				causeKind: "DEPLOYMENT_CAUSE_KIND_USER" as const,
				causeId: "user-1",
				reasonCode: "DEPLOYMENT_REMOVED",
				detail: "Service removed",
				specRevision: 1,
				imageDigest: "sha256:removed",
				rolloutGeneration: 1,
			},
		};

		expect(isPinnedDeployment(removed)).toBe(false);
		expect(partitionDeployments([removed])).toEqual({
			live: [],
			history: [removed],
		});
	});

	it("keeps a draining predecessor live until it is actually stopped", () => {
		const incoming = {
			id: "deploy-2",
			rolloutGeneration: 2,
			isCurrent: true,
			status: {
				deploymentId: "deploy-2",
				state: "DEPLOYMENT_STATE_BUILDING" as const,
				causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK" as const,
				causeId: "hook",
				reasonCode: "BUILD_STARTED",
				detail: "Building image",
				specRevision: 2,
				imageDigest: "",
				rolloutGeneration: 2,
			},
		};
		const stillServing = {
			id: "deploy-1",
			rolloutGeneration: 1,
			isCurrent: false,
			status: {
				deploymentId: "deploy-1",
				state: "DEPLOYMENT_STATE_ACTIVE" as const,
				causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK" as const,
				causeId: "hook",
				reasonCode: "DEPLOYMENT_ACTIVE",
				detail: "Serving traffic",
				specRevision: 1,
				imageDigest: "sha256:old",
				rolloutGeneration: 1,
			},
		};
		const stopped = {
			id: "deploy-0",
			rolloutGeneration: 0,
			isCurrent: false,
			status: {
				deploymentId: "deploy-0",
				state: "DEPLOYMENT_STATE_COMPLETED" as const,
				causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK" as const,
				causeId: "hook",
				reasonCode: "DEPLOYMENT_COMPLETED",
				detail: "Replaced",
				specRevision: 1,
				imageDigest: "sha256:older",
				rolloutGeneration: 0,
			},
		};

		expect(isPinnedDeployment(stillServing)).toBe(true);
		const { live, history } = partitionDeployments([
			incoming,
			stillServing,
			stopped,
		]);
		expect(live.map((entry) => entry.id)).toEqual(["deploy-2", "deploy-1"]);
		expect(history.map((entry) => entry.id)).toEqual(["deploy-0"]);
	});
});

describe("deploymentProgressCopy", () => {
	it("names the live step instead of listing the pipeline", () => {
		expect(
			deploymentProgressCopy({
				status: {
					deploymentId: "deploy-2",
					state: "DEPLOYMENT_STATE_BUILDING",
					causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK",
					causeId: "hook",
					reasonCode: "BUILD_STARTED",
					detail: "Publishing image",
					specRevision: 2,
					imageDigest: "",
					rolloutGeneration: 2,
				},
				stages: [
					{
						key: "source",
						label: "Source",
						detail: "",
						state: "DEPLOYMENT_STAGE_STATE_SUCCEEDED",
					},
					{
						key: "build",
						label: "Build",
						detail: "Building",
						state: "DEPLOYMENT_STAGE_STATE_RUNNING",
					},
					{
						key: "deploy",
						label: "Deploy",
						detail: "",
						state: "DEPLOYMENT_STAGE_STATE_PENDING",
					},
				],
				stepHint: "step 6 of 7",
			}),
		).toBe("Deployment in progress: Publishing image · step 6 of 7");
	});

	it("treats an active rollout as successful", () => {
		expect(
			deploymentProgressCopy({
				status: {
					deploymentId: "deploy-1",
					state: "DEPLOYMENT_STATE_ACTIVE",
					causeKind: "DEPLOYMENT_CAUSE_KIND_WEBHOOK",
					causeId: "hook",
					reasonCode: "DEPLOYMENT_ACTIVE",
					detail: "Serving traffic",
					specRevision: 1,
					imageDigest: "sha256:ok",
					rolloutGeneration: 1,
				},
				stages: [],
			}),
		).toBe("Deployment successful");
	});
});

describe("deploymentBadgeLabel", () => {
	it("uses the deployment state for the compact badge", () => {
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_BUILDING")).toBe("Building");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_ACTIVE")).toBe("Healthy");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_DRAINING")).toBe("Draining");
	});

	it("distinguishes every user-visible deployment state", () => {
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_STAGED")).toBe("Staged");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_QUEUED_BUILD")).toBe(
			"Queued",
		);
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_BUILDING")).toBe("Building");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_SCHEDULING")).toBe(
			"Deploying",
		);
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_IMAGE_PULL")).toBe(
			"Deploying",
		);
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_STARTING")).toBe("Deploying");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_READINESS")).toBe(
			"Deploying",
		);
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_ACTIVE")).toBe("Healthy");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_FAILED")).toBe("Unhealthy");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_CRASHED")).toBe("Crashed");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_CANCELLED")).toBe(
			"Cancelled",
		);
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_REMOVED")).toBe("Removed");
		expect(deploymentBadgeLabel("DEPLOYMENT_STATE_SUPERSEDED")).toBe(
			"Superseded",
		);
		expect(deploymentBadgeLabel(undefined)).toBe("Not deployed");
	});
});
