import type * as stylex from "@stylexjs/stylex";
import {
	ProgressRail,
	type ProgressState,
} from "#/components/ui/progress-rail";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

type Stage = NonNullable<
	DashboardServiceRecord["latestBuild"]
>["stages"][number];
const stageStates: Record<Stage["state"], ProgressState> = {
	DEPLOYMENT_STAGE_STATE_UNSPECIFIED: "queued",
	DEPLOYMENT_STAGE_STATE_PENDING: "queued",
	DEPLOYMENT_STAGE_STATE_RUNNING: "running",
	DEPLOYMENT_STAGE_STATE_SUCCEEDED: "succeeded",
	DEPLOYMENT_STAGE_STATE_FAILED: "failed",
	DEPLOYMENT_STAGE_STATE_SKIPPED: "succeeded",
};
export function DeploymentProgress({
	stages,
	...props
}: {
	stages: readonly Stage[];
	label: string;
	completing?: boolean;
	delay?: string;
	styles?: stylex.StyleXStyles;
}) {
	return (
		<ProgressRail
			{...props}
			building
			steps={stages.map((stage) => ({
				id: stage.key || stage.label,
				label: stage.label || stage.key,
				state: stageStates[stage.state],
			}))}
		/>
	);
}
