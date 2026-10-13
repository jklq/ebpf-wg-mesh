import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function WorkspacePageHeader({
	title,
	lede,
	actions,
}: {
	title: string;
	lede?: ReactNode;
	actions?: ReactNode;
}) {
	return (
		<header {...stylex.props(styles.header)}>
			<div {...stylex.props(styles.text)}>
				<h1 {...stylex.props(styles.title)}>{title}</h1>
				{lede && <p {...stylex.props(styles.lede)}>{lede}</p>}
			</div>
			{actions && <div {...stylex.props(styles.actions)}>{actions}</div>}
		</header>
	);
}

const styles = stylex.create({
	header: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "flex-end",
		justifyContent: "space-between",
		gap: space.lg,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderBottomColor: colors.line,
		paddingBottom: space.xl,
	},
	text: { display: "flex", flexDirection: "column", gap: space.xs },
	title: {
		margin: 0,
		fontFamily: fonts.display,
		fontSize: "36px",
		fontWeight: "500",
		letterSpacing: "-0.02em",
		color: colors.ink,
	},
	lede: { margin: 0, fontSize: "13px", color: colors.muted },
	actions: { display: "flex", alignItems: "center", gap: space.sm },
});
