import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { Badge } from "#/components/ui/badge";
import { Button } from "#/components/ui/button";
import { CommitContributors } from "#/features/dashboard/service-panel/deployments/commit-contributors";
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
	const directImage = record.artifact?.kind === "direct_image";
	return (
		<div {...stylex.props(styles.header)}>
			<Badge tone={health} styles={tone === "draining" && styles.drainingBadge}>
				{deploymentBadgeLabel(record.status?.state, record.build)}
			</Badge>
			{!directImage && (
				<CommitContributors contributors={record.build?.commitContributors} />
			)}
			<button
				type="button"
				onClick={onOpenLogs}
				disabled={!logsEnabled}
				{...stylex.props(styles.summary)}
			>
				<p {...stylex.props(styles.headline)}>
					{directImage
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
				<DeploymentProgress
					stages={stages}
					label="Deploy steps"
					styles={styles.progress}
					segmentStyles={styles.progressSegment}
				/>
			)}
			<Button
				variant="secondary"
				onClick={onOpenLogs}
				disabled={!logsEnabled}
				styles={[
					styles.logsButton,
					tone === "active" && styles.healthyLogs,
					tone === "running" && styles.buildingLogs,
					tone === "failed" && styles.failedLogs,
					tone === "draining" && styles.completedLogs,
					!tone && styles.neutralLogs,
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
	progress: { width: 64 },
	progressSegment: {
		transitionDuration: "280ms",
		transitionTimingFunction: "ease-in-out",
	},
	logsButton: {
		minHeight: 32,
		flexShrink: 0,
		paddingInline: 12,
		paddingBlock: 0,
		fontWeight: 700,
		cursor: { default: "pointer", ":disabled": "default" },
		opacity: { default: 1, ":disabled": 0.5 },
		transitionProperty: "none",
		fontFamily: fonts.condensed,
		fontSize: 11,
		letterSpacing: "0.09em",
		width: { default: "auto", "@media (max-width: 899px)": "100%" },
	},
	healthyLogs: {
		borderColor: {
			default: "rgba(109,190,130,0.38)",
			":enabled:hover": "rgba(109,190,130,0.62)",
			":focus-visible": "rgba(109,190,130,0.62)",
			[stylex.when.ancestor(":hover")]: "rgba(109,190,130,0.62)",
		},
		backgroundColor: {
			default: "rgba(109,190,130,0.1)",
			":enabled:hover": "rgba(109,190,130,0.2)",
			":focus-visible": "rgba(109,190,130,0.2)",
			[stylex.when.ancestor(":hover")]: "rgba(109,190,130,0.2)",
		},
		color: {
			default: colors.healthy,
			":enabled:hover": "#9ad6aa",
			":focus-visible": "#9ad6aa",
			[stylex.when.ancestor(":hover")]: "#9ad6aa",
		},
	},
	buildingLogs: {
		borderColor: {
			default: "rgba(212,154,42,0.4)",
			":enabled:hover": "rgba(212,154,42,0.64)",
			":focus-visible": "rgba(212,154,42,0.64)",
			[stylex.when.ancestor(":hover")]: "rgba(212,154,42,0.64)",
		},
		backgroundColor: {
			default: "rgba(212,154,42,0.1)",
			":enabled:hover": "rgba(212,154,42,0.2)",
			":focus-visible": "rgba(212,154,42,0.2)",
			[stylex.when.ancestor(":hover")]: "rgba(212,154,42,0.2)",
		},
		color: {
			default: colors.building,
			":enabled:hover": "#e4b65a",
			":focus-visible": "#e4b65a",
			[stylex.when.ancestor(":hover")]: "#e4b65a",
		},
	},
	completedLogs: {
		borderColor: {
			default: "rgba(125,117,107,0.42)",
			":enabled:hover": "rgba(183,174,162,0.45)",
			":focus-visible": "rgba(183,174,162,0.45)",
			[stylex.when.ancestor(":hover")]: "rgba(183,174,162,0.45)",
		},
		backgroundColor: {
			default: "rgba(80,76,71,0.22)",
			":enabled:hover": "rgba(108,102,94,0.32)",
			":focus-visible": "rgba(108,102,94,0.32)",
			[stylex.when.ancestor(":hover")]: "rgba(108,102,94,0.32)",
		},
		color: {
			default: colors.muted,
			":enabled:hover": colors.ink,
			":focus-visible": colors.ink,
			[stylex.when.ancestor(":hover")]: colors.ink,
		},
	},
	failedLogs: {
		borderColor: {
			default: "rgba(208,85,85,0.38)",
			":enabled:hover": "rgba(208,85,85,0.6)",
			":focus-visible": "rgba(208,85,85,0.6)",
			[stylex.when.ancestor(":hover")]: "rgba(208,85,85,0.6)",
		},
		backgroundColor: {
			default: "rgba(208,85,85,0.1)",
			":enabled:hover": "rgba(208,85,85,0.2)",
			":focus-visible": "rgba(208,85,85,0.2)",
			[stylex.when.ancestor(":hover")]: "rgba(208,85,85,0.2)",
		},
		color: {
			default: colors.failed,
			":enabled:hover": "#e08989",
			":focus-visible": "#e08989",
			[stylex.when.ancestor(":hover")]: "#e08989",
		},
	},
	neutralLogs: {
		borderColor: colors.line,
		backgroundColor: {
			default: "rgba(255,255,255,0.03)",
			":enabled:hover": colors.surfaceHover,
			":focus-visible": colors.surfaceHover,
		},
		color: colors.ink,
	},
});
