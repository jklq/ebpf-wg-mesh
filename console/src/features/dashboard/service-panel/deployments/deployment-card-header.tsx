import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { Badge } from "#/components/ui/badge";
import { Button } from "#/components/ui/button";
import { deploymentBadgeLabel } from "#/features/dashboard/service-panel/deployments/deployment-inline";
import { DeploymentProgress } from "#/features/dashboard/service-panel/deployments/deployment-progress";
import {
	type DeploymentCardTone,
	deploymentCardHeadline,
	toneToHealth,
} from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import type { DashboardDeploymentRecord } from "#/lib/dashboard/core/types.server";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function DeploymentCardHeader({
	record,
	tone,
	metadata,
	stages,
	logsEnabled,
	onOpenLogs,
	actions,
}: {
	record: DashboardDeploymentRecord;
	tone: DeploymentCardTone | undefined;
	metadata: readonly string[];
	stages: NonNullable<DashboardDeploymentRecord["build"]>["stages"];
	logsEnabled: boolean;
	onOpenLogs: () => void;
	actions: ReactNode;
}) {
	const health = tone ? toneToHealth(tone) : "healthy";
	return (
		<div {...stylex.props(styles.header)}>
			<Badge tone={health} styles={tone === "draining" && styles.drainingBadge}>
				{deploymentBadgeLabel(record.status?.state, record.build)}
			</Badge>
			<button
				type="button"
				onClick={onOpenLogs}
				disabled={!logsEnabled}
				{...stylex.props(styles.summary)}
			>
				<p {...stylex.props(styles.headline)}>
					{record.artifact?.kind === "direct_image"
						? "Image deployment"
						: deploymentCardHeadline(record.build)}
				</p>
				<div {...stylex.props(styles.metadata)}>
					{metadata.map((entry, index) => (
						<span key={entry} {...stylex.props(styles.metadataEntry)}>
							{index > 0 && <span {...stylex.props(styles.separator)} />}
							{entry}
						</span>
					))}
				</div>
			</button>
			{tone === "running" && stages.length > 0 && (
				<DeploymentProgress stages={stages} label="Deploy steps" />
			)}
			<Button
				variant="secondary"
				onClick={onOpenLogs}
				disabled={!logsEnabled}
				styles={[
					styles.logsButton,
					health === "healthy" && styles.healthyLogs,
					health === "building" && styles.buildingLogs,
					health === "failed" && styles.failedLogs,
				]}
			>
				View logs
			</Button>
			<div {...stylex.props(styles.actions)}>{actions}</div>
		</div>
	);
}

const styles = stylex.create({
	header: {
		position: "relative",
		display: "flex",
		minHeight: 52,
		alignItems: { default: "center", "@media (max-width: 899px)": "stretch" },
		gap: space.md,
		paddingInline: { default: 28, "@media (max-width: 899px)": space.lg },
		paddingBlock: 10,
		flexDirection: { default: "row", "@media (max-width: 899px)": "column" },
	},
	drainingBadge: {
		borderColor: "rgba(80,76,71,0.28)",
		backgroundColor: "rgba(80,76,71,0.18)",
		color: colors.dim,
	},
	summary: {
		display: "flex",
		minWidth: 0,
		flex: 1,
		cursor: { default: "pointer", ":disabled": "default" },
		flexDirection: "column",
		gap: space.xs,
		border: 0,
		backgroundColor: "transparent",
		padding: 0,
		textAlign: "left",
	},
	headline: {
		margin: 0,
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontFamily: fonts.display,
		fontSize: 15,
		fontWeight: 500,
		lineHeight: 1.25,
		letterSpacing: "-0.02em",
		color: colors.ink,
	},
	metadata: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: 7,
		fontFamily: fonts.mono,
		fontSize: 11,
		color: colors.muted,
	},
	metadataEntry: { display: "inline-flex", alignItems: "center", gap: 7 },
	separator: {
		display: "inline-block",
		width: 3,
		height: 3,
		flexShrink: 0,
		borderRadius: 999,
		backgroundColor: colors.dim,
	},
	actions: {
		position: { default: "relative", "@media (max-width: 899px)": "absolute" },
		top: { default: 0, "@media (max-width: 899px)": 10 },
		right: { default: 0, "@media (max-width: 899px)": 28 },
	},
	logsButton: {
		minHeight: 32,
		fontFamily: fonts.condensed,
		fontSize: 11,
		letterSpacing: "0.09em",
		width: { default: "auto", "@media (max-width: 899px)": "100%" },
	},
	healthyLogs: {
		borderColor: {
			default: "rgba(109,190,130,0.38)",
			":hover": "rgba(109,190,130,0.62)",
		},
		backgroundColor: {
			default: colors.healthyDim,
			":hover": "rgba(109,190,130,0.2)",
		},
		color: colors.healthy,
	},
	buildingLogs: {
		borderColor: {
			default: "rgba(192,133,32,0.34)",
			":hover": "rgba(192,133,32,0.5)",
		},
		backgroundColor: {
			default: colors.buildingDim,
			":hover": "rgba(192,133,32,0.2)",
		},
		color: colors.building,
	},
	failedLogs: {
		borderColor: {
			default: "rgba(208,85,85,0.34)",
			":hover": "rgba(208,85,85,0.6)",
		},
		backgroundColor: {
			default: colors.failedDim,
			":hover": "rgba(208,85,85,0.2)",
		},
		color: colors.failed,
	},
});
