import { describe, expect, it } from "vitest";

import {
	buildStepHint,
	deploymentBadgeLabel,
	deploymentProgressCopy,
	extractMissingEnvKeys,
	isPinnedDeployment,
	partitionDeployments,
	selectInlineLogSnippet,
	stageAttemptLabel,
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

describe("stageAttemptLabel", () => {
	it("marks later stages as not attempted after a failure", () => {
		expect(
			stageAttemptLabel({
				state: "pending",
				detail: "Waiting for build to finish",
				priorFailed: true,
			}),
		).toBe("not attempted");
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
	it("keeps a draining predecessor live until it is actually stopped", () => {
		const incoming = {
			id: "deploy-2",
			rolloutGeneration: 2,
			isCurrent: true,
			status: {
				deploymentId: "deploy-2",
				state: "building" as const,
				causeKind: "webhook" as const,
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
				state: "active" as const,
				causeKind: "webhook" as const,
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
				state: "completed" as const,
				causeKind: "webhook" as const,
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
					state: "building",
					causeKind: "webhook",
					causeId: "hook",
					reasonCode: "BUILD_STARTED",
					detail: "Publishing image",
					specRevision: 2,
					imageDigest: "",
					rolloutGeneration: 2,
				},
				stages: [
					{ key: "source", label: "Source", detail: "", state: "succeeded" },
					{
						key: "build",
						label: "Build",
						detail: "Building",
						state: "running",
					},
					{ key: "deploy", label: "Deploy", detail: "", state: "pending" },
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
					state: "active",
					causeKind: "webhook",
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
		expect(deploymentBadgeLabel("building")).toBe("Building");
		expect(deploymentBadgeLabel("active")).toBe("Active");
		expect(deploymentBadgeLabel("draining")).toBe("Draining");
	});
});
