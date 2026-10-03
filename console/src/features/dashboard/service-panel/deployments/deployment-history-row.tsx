import * as stylex from "@stylexjs/stylex";
import { useState } from "react";
import { noticeStyles } from "#/components/ui/notice";
import { statusDotStylesFor } from "#/components/ui/status-dot";
import { CommitContributors } from "#/features/dashboard/service-panel/deployments/commit-contributors";
import { DeploymentActionsMenu } from "#/features/dashboard/service-panel/deployments/deployment-actions-menu";
import { DeploymentDetails } from "#/features/dashboard/service-panel/deployments/deployment-details";
import {
	actionLabel,
	availableDeploymentActions,
	deploymentCardHeadline,
	getDeploymentCardTone,
	hasActiveDeployment,
	toneToHealth,
} from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import { shortSha } from "#/features/dashboard/shared/service-utils";
import type {
	DashboardAllocationStatus,
	DashboardBuildStatus,
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDeploymentStatus,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/errors";
import { formatRelativeTime, NOT_DEPLOYED_LABEL } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	row: {
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: "rgba(80,76,71,0.35)",
	},
	summary: { display: "flex", alignItems: "center" },
	openLogsButton: {
		display: "grid",
		minHeight: "42px",
		width: "100%",
		minWidth: "0rem",
		cursor: { default: "pointer", ":disabled": "default" },
		gridTemplateColumns: "24px minmax(0,1fr) auto",
		alignItems: "center",
		gap: "0.625rem",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: {
			default: "transparent",
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(255,255,255,0.025)",
			},
		},
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBlock: space.sm,
		textAlign: "left",
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
		opacity: { default: null, ":disabled": "55%" },
	},
	title: {
		display: "flex",
		minWidth: "0rem",
		alignItems: "center",
		gap: space.sm,
	},
	headline: {
		minWidth: "0rem",
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontSize: "13px",
	},
	metadata: {
		display: "inline-flex",
		alignItems: "center",
		gap: space.sm,
		fontFamily: fonts.mono,
		fontSize: "10px",
		color: colors.dim,
	},
	commitSha: { fontFamily: fonts.mono },
	actions: {
		marginRight: { default: "1.75rem", "@media (width < 900px)": "1rem" },
	},
	errorMessage: {
		marginRight: "1.75rem",
		marginBottom: space.sm,
		marginLeft: "34px",
		paddingInline: "9px",
		paddingBlock: "7px",
		fontSize: "11px",
	},
});
export function DeploymentHistoryRow({
	serviceId,
	record,
	build,
	allocation,
	status,
	logsEnabled,
	nowMs,
	deploymentInProgress,
	onOpenLogs,
	onAction,
}: {
	serviceId?: string;
	record: DashboardDeploymentRecord;
	build: DashboardBuildStatus | undefined;
	allocation: DashboardAllocationStatus | undefined;
	status?: DashboardDeploymentStatus;
	logsEnabled: boolean;
	nowMs: number;
	deploymentInProgress: boolean;
	onOpenLogs: () => void;
	onAction: (
		record: DashboardDeploymentRecord,
		action: DashboardDeploymentAction,
	) => Promise<void>;
}) {
	const tone =
		getDeploymentCardTone({
			build,
			allocation,
			active: hasActiveDeployment(status, build),
			isCurrent: false,
			status,
		}) ?? "draining";
	const timestamp =
		build?.startedAt ??
		build?.queuedAt ??
		build?.finishedAt ??
		allocation?.updatedAt;
	const [pendingAction, setPendingAction] =
		useState<DashboardDeploymentAction>();
	const [actionError, setActionError] = useState<string>();
	const actions = availableDeploymentActions(record, deploymentInProgress);

	const runAction = async (action: DashboardDeploymentAction) => {
		if (pendingAction) return;
		setPendingAction(action);
		setActionError(undefined);
		try {
			await onAction(record, action);
		} catch (cause) {
			setActionError(
				formatError(cause, `Unable to ${actionLabel(action).toLowerCase()}.`),
			);
		} finally {
			setPendingAction(undefined);
		}
	};

	return (
		<div {...stylex.props(styles.row)}>
			<div {...stylex.props(styles.summary)}>
				<button
					type="button"
					{...stylex.props(styles.openLogsButton)}
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					<span
						role="img"
						{...stylex.props(statusDotStylesFor(toneToHealth(tone)))}
						aria-label={`Status: ${toneToHealth(tone)}`}
					/>
					{record.artifact?.kind === "direct_image" ? (
						<span {...stylex.props(styles.headline)}>Image deployment</span>
					) : (
						<span {...stylex.props(styles.title)}>
							<CommitContributors
								contributors={build?.commitContributors}
								size="sm"
							/>
							<span {...stylex.props(styles.headline)}>
								{deploymentCardHeadline(build)}
							</span>
						</span>
					)}
					<span {...stylex.props(styles.metadata)}>
						{build?.commitSha && (
							<span {...stylex.props(styles.commitSha)}>
								{shortSha(build.commitSha)}
							</span>
						)}
						<span>
							{timestamp
								? formatRelativeTime(timestamp, nowMs)
								: NOT_DEPLOYED_LABEL}
						</span>
					</span>
				</button>
				{actions.length > 0 && (
					<DeploymentActionsMenu
						styles={styles.actions}
						actions={actions}
						pendingAction={pendingAction}
						onAction={runAction}
					/>
				)}
			</div>
			<DeploymentDetails record={record} serviceId={serviceId} />
			{actionError && (
				<div {...stylex.props([noticeStyles.error, styles.errorMessage])}>
					{actionError}
				</div>
			)}
		</div>
	);
}
