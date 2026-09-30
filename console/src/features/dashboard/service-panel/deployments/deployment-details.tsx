import * as stylex from "@stylexjs/stylex";
import { useEffect, useState } from "react";
import { noticeStyles } from "#/components/ui/notice";
import type {
	DashboardBuildAttempt,
	DashboardDeploymentRecord,
} from "#/lib/dashboard/core/types.server";
import { fetchBuildAttempts } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function DeploymentDetails({
	record,
	serviceId,
}: {
	record: DashboardDeploymentRecord;
	serviceId?: string;
}) {
	const artifact = record.artifact ?? record.build?.artifact;
	const [open, setOpen] = useState(false);
	const [attempts, setAttempts] = useState<DashboardBuildAttempt[]>([]);
	const [error, setError] = useState<string>();
	const buildId = record.build?.buildId;
	const attemptCount = record.build?.attemptCount;
	const buildState = record.build?.state;
	useEffect(() => {
		// A new attempt invalidates the history while it is open.
		void attemptCount;
		void buildState;
		if (!open || !serviceId || !buildId) return;
		let current = true;
		void fetchBuildAttempts({ data: { serviceId, buildId } })
			.then((result) => {
				if (current) {
					setAttempts(result);
					setError(undefined);
				}
			})
			.catch((cause) => {
				if (current) setError(formatError(cause));
			});
		return () => {
			current = false;
		};
	}, [open, serviceId, buildId, attemptCount, buildState]);
	const reused =
		record.status?.buildReused || record.status?.reasonCode === "BUILD_REUSED";
	return (
		<div {...stylex.props(styles.details)}>
			{artifact?.imageRef && (
				<div {...stylex.props(styles.imageReference)}>
					<span {...stylex.props(styles.detailLabel)}>Image: </span>
					{artifact.sourceImageRef && <>{artifact.sourceImageRef} → </>}
					{artifact.imageRef}
				</div>
			)}
			{reused && (
				<p>
					Reusing image built for commit{" "}
					{artifact?.commitSha ?? record.build?.commitSha ?? "unknown"}.
				</p>
			)}
			{Object.keys(record.sealedVersions ?? {}).length > 0 && (
				<details>
					<summary {...stylex.props(styles.secretsSummary)}>
						Pinned secret versions · restored on rollback
					</summary>
					<ul>
						{Object.entries(record.sealedVersions ?? {}).map(
							([name, version]) => (
								<li key={name} {...stylex.props(styles.imageReference)}>
									{name}: v{version}
								</li>
							),
						)}
					</ul>
				</details>
			)}
			{serviceId && buildId && !reused && (
				<details open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
					<summary {...stylex.props(styles.secretsSummary)}>
						Build attempts {attemptCount ?? 0}/
						{record.build?.attemptLimit ?? "—"}
					</summary>
					{error && (
						<p role="alert" {...stylex.props(noticeStyles.error)}>
							{error}
						</p>
					)}
					{attempts.map((attempt) => (
						<div
							key={attempt.attemptNumber}
							{...stylex.props(styles.buildAttempt)}
						>
							<strong>Attempt {attempt.attemptNumber}</strong> ·{" "}
							{attempt.builderId} · {attempt.outcome.replaceAll("_", " ")}
							<p {...stylex.props(styles.attemptDetail)}>{attempt.detail}</p>
						</div>
					))}
					{!attempts.length && !error && <p>No attempts recorded.</p>}
				</details>
			)}
		</div>
	);
}

const styles = stylex.create({
	details: {
		display: "flex",
		flexDirection: "column",
		gap: space.sm,
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBottom: space.md,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	imageReference: { wordBreak: "break-all", fontFamily: fonts.mono },
	detailLabel: { fontFamily: fonts.sans },
	secretsSummary: { cursor: "pointer" },
	buildAttempt: {
		marginTop: space.sm,
		borderLeftStyle: "solid",
		borderLeftWidth: "1px",
		borderColor: colors.line,
		paddingLeft: space.md,
	},
	attemptDetail: { whiteSpace: "pre-wrap" },
});
