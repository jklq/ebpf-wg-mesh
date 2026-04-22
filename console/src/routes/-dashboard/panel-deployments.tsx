import { Loader2 } from "lucide-react";

import type {
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { buildBadgeClass, shortId, shortSha } from "./service-utils";

export function PanelDeployments({
	service,
	status,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
}) {
	const build = status?.service.latestBuild ?? service.latestBuild;

	return (
		<div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
			<p className="section-header">Build history</p>

			{!build && (
				<p style={{ fontSize: 13, color: "var(--text-muted)" }}>
					No builds yet. A build starts automatically after the source is
					configured.
				</p>
			)}

			{build && (
				<div className="deployment-card">
					<div
						style={{
							display: "flex",
							alignItems: "center",
							justifyContent: "space-between",
						}}
					>
						<span className={`badge ${buildBadgeClass(build.state)}`}>
							{build.state === "running" && (
								<Loader2
									size={10}
									style={{ animation: "spin 1s linear infinite" }}
								/>
							)}
							{build.state}
						</span>
						<span
							style={{
								fontSize: 10,
								fontFamily: "var(--font-mono)",
								color: "var(--text-dim)",
							}}
						>
							{shortId(build.buildId)}
						</span>
					</div>

					<div>
						<div className="info-row">
							<span className="info-row-label">Commit</span>
							<span className="info-row-value">
								{build.commitSha ? shortSha(build.commitSha) : "—"}
							</span>
						</div>
						{build.imageDigest && (
							<div className="info-row">
								<span className="info-row-label">Image</span>
								<span
									className="info-row-value"
									title={build.imageDigest}
									style={{ color: "var(--text-muted)" }}
								>
									{build.imageDigest.slice(0, 24)}…
								</span>
							</div>
						)}
						{build.failureReason && (
							<div className="info-row">
								<span className="info-row-label">Failure</span>
								<span
									className="info-row-value"
									style={{ color: "var(--failed)" }}
								>
									{build.failureReason}
								</span>
							</div>
						)}
					</div>
				</div>
			)}

			<p
				style={{
					fontSize: 11,
					color: "var(--text-dim)",
					margin: 0,
					fontStyle: "italic",
				}}
			>
				Full build history will be available in a future release.
			</p>
		</div>
	);
}
