import { useEffect, useState } from "react";
import type {
	DashboardBuildAttempt,
	DashboardDeploymentRecord,
} from "#/lib/dashboard/core/types.server";
import { errorMsg } from "#/lib/ui-classes";
import { fetchBuildAttempts } from "./server-fns";
import { formatError } from "./service-utils";

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
		<div className="space-y-2 px-7 pb-3 text-xs text-muted max-[900px]:px-4">
			{artifact?.imageRef && (
				<div className="break-all font-mono">
					<span className="font-sans">Image: </span>
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
					<summary className="cursor-pointer">
						Pinned secret versions · restored on rollback
					</summary>
					<ul>
						{Object.entries(record.sealedVersions ?? {}).map(
							([name, version]) => (
								<li key={name} className="break-all font-mono">
									{name}: v{version}
								</li>
							),
						)}
					</ul>
				</details>
			)}
			{serviceId && buildId && !reused && (
				<details open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
					<summary className="cursor-pointer">
						Build attempts {attemptCount ?? 0}/
						{record.build?.attemptLimit ?? "—"}
					</summary>
					{error && (
						<p role="alert" className={errorMsg}>
							{error}
						</p>
					)}
					{attempts.map((attempt) => (
						<div
							key={attempt.attemptNumber}
							className="mt-2 border-l border-line pl-3"
						>
							<strong>Attempt {attempt.attemptNumber}</strong> ·{" "}
							{attempt.builderId} · {attempt.outcome.replaceAll("_", " ")}
							<p className="whitespace-pre-wrap">{attempt.detail}</p>
						</div>
					))}
					{!attempts.length && !error && <p>No attempts recorded.</p>}
				</details>
			)}
		</div>
	);
}
