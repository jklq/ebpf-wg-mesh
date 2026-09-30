import * as stylex from "@stylexjs/stylex";
import { actionLabel } from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import { shortId } from "#/features/dashboard/shared/service-utils";
import type { DashboardDeploymentRecord } from "#/lib/dashboard/core/types.server";
import { colors, fonts } from "#/styles/tokens.stylex";

const styles = stylex.create({
	history: {
		display: "flex",
		width: "100%",
		flexWrap: "wrap",
		alignItems: "center",
		gap: "0.375rem",
		fontFamily: fonts.mono,
		fontSize: "10px",
		color: colors.dim,
	},
	actionTag: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(80,76,71,0.45)",
		paddingInline: "5px",
		paddingBlock: "0.125rem",
	},
});
export function DeploymentActionHistory({
	record,
}: {
	record: DashboardDeploymentRecord;
}) {
	if (!record.actions?.length) return null;
	return (
		<section {...stylex.props(styles.history)} aria-label="Deployment actions">
			{record.actions.map((action) => (
				<span key={action.id} {...stylex.props(styles.actionTag)}>
					{actionLabel(action.action)}
					{action.allocationId ? ` ${shortId(action.allocationId)}` : ""}
				</span>
			))}
		</section>
	);
}
