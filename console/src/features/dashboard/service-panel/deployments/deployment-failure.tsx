import * as stylex from "@stylexjs/stylex";
import { Button } from "#/components/ui/button";
import type { selectInlineLogSnippet } from "#/features/dashboard/service-panel/deployments/deployment-inline";
import type { logLinesForStage } from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import type {
	DashboardDeploymentAction,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const stageInlineIn = stylex.keyframes({
	from: { opacity: "0", transform: "translateY(-4px)" },
	to: { opacity: "1", transform: "none" },
});
const styles = stylex.create({
	failureDetails: {
		display: "flex",
		animation: `${stageInlineIn} 0.22s cubic-bezier(0.25, 0.46, 0.45, 0.94) both`,
		flexDirection: "column",
		gap: "0.625rem",
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBottom: "0.875rem",
	},
	logSnippet: {
		overflow: "hidden",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(80,76,71,0.55)",
		backgroundColor: "#10100f",
		fontFamily: fonts.mono,
		fontSize: "11px",
		lineHeight: "1.6",
		boxShadow: "inset 0 1px 0 rgba(255,255,255,0.02)",
	},
	logLine: {
		paddingInline: "0.625rem",
		paddingBlock: "3px",
		whiteSpace: "pre-wrap",
		color: colors.dim,
		overflowWrap: "anywhere",
	},
	emphasizedLogLine: {
		backgroundColor: "rgba(184,66,66,0.16)",
		color: "#d27a7a",
		boxShadow: `inset 2px 0 0 ${colors.failed}`,
	},
	missingVariables: { display: "flex", flexWrap: "wrap", gap: space.sm },
	addVariableButton: { minHeight: "1.75rem", paddingInline: "11px" },
});
export function DeploymentFailure({
	stage,
	snippet,
	sourceLines,
	missingKeys,
	logsEnabled,
	pendingAction,
	onOpenLogs,
	onOpenVariables,
	onRetry,
}: {
	stage: NonNullable<DashboardServiceRecord["latestBuild"]>["stages"][number];
	snippet: ReturnType<typeof selectInlineLogSnippet>;
	sourceLines: ReturnType<typeof logLinesForStage>;
	missingKeys: string[];
	logsEnabled: boolean;
	pendingAction: DashboardDeploymentAction | undefined;
	onOpenLogs: () => void;
	onOpenVariables?: (key: string) => void;
	onRetry: () => void;
}) {
	return (
		<div {...stylex.props(styles.failureDetails)}>
			{snippet.lines.length > 0 && (
				<div
					{...stylex.props(styles.logSnippet)}
					role="log"
					aria-label={`${stage.label || stage.key} logs`}
				>
					{snippet.lines.map((line, lineIndex) => {
						const source = sourceLines[snippet.startIndex + lineIndex];
						return (
							<div
								key={`${source?.sequence ?? "fallback"}:${source?.observedAt ?? "local"}:${line}`}
								{...stylex.props([
									styles.logLine,
									snippet.highlightIndexes.includes(lineIndex) &&
										styles.emphasizedLogLine,
								])}
							>
								{line}
							</div>
						);
					})}
				</div>
			)}
			<div {...stylex.props(styles.missingVariables)}>
				{missingKeys.map((key) => (
					<Button
						key={key}
						type="button"
						variant="primary"
						styles={[styles.addVariableButton]}
						onClick={() => onOpenVariables?.(key)}
					>
						Add {key}
					</Button>
				))}
				<Button
					type="button"
					variant="secondary"
					styles={[styles.addVariableButton]}
					onClick={onOpenLogs}
					disabled={!logsEnabled}
				>
					Full log
				</Button>
				<Button
					type="button"
					variant="secondary"
					styles={[styles.addVariableButton]}
					onClick={onRetry}
					disabled={Boolean(pendingAction)}
				>
					{pendingAction === "DEPLOYMENT_ACTION_RETRY"
						? "Retrying…"
						: "Retry build"}
				</Button>
			</div>
		</div>
	);
}
