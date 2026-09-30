import * as stylex from "@stylexjs/stylex";
import { Layers } from "lucide-react";
import { DeployButton } from "#/features/dashboard/navigation/deploy-button";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

export function EmptyCanvas({
	state,
	onAdd,
	onPreloadAdd,
}: {
	state: DashboardHomeState;
	onAdd: () => void;
	onPreloadAdd?: () => void;
}) {
	return (
		<div {...stylex.props(styles.emptyState)}>
			<div {...stylex.props(styles.iconFrame)}>
				<Layers size={24} {...stylex.props(styles.icon)} />
			</div>
			<div {...stylex.props(styles.copy)}>
				<p {...stylex.props(styles.title)}>No services</p>
				<p {...stylex.props(styles.description)}>
					Deploy your first service from a GitHub repo
				</p>
			</div>
			<div {...stylex.props(styles.actions)}>
				<DeployButton
					state={state}
					onNewService={onAdd}
					onPreload={onPreloadAdd}
					stopPropagation
				/>
			</div>
		</div>
	);
}

const styles = stylex.create({
	emptyState: {
		pointerEvents: "none",
		position: "absolute",
		inset: "0rem",
		zIndex: "20",
		display: "flex",
		height: "100%",
		flexDirection: "column",
		alignItems: "center",
		justifyContent: "center",
		gap: space.lg,
		color: colors.muted,
	},
	actions: { pointerEvents: "auto" },
	iconFrame: {
		display: "flex",
		width: "3.5rem",
		height: "3.5rem",
		alignItems: "center",
		justifyContent: "center",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
	},
	icon: { color: colors.dim },
	copy: { textAlign: "center" },
	title: {
		margin: "0rem",
		marginBottom: space.xs,
		fontFamily: fonts.condensed,
		fontSize: "15px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.08em",
		color: colors.ink,
	},
	description: {
		margin: "0rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
});
