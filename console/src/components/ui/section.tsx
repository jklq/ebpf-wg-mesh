import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { colors, fonts, space } from "#/styles/tokens.stylex";
export function PanelSection({
	title,
	lede,
	tone = "default",
	children,
}: {
	title: string;
	lede?: ReactNode;
	tone?: "default" | "danger";
	children: ReactNode;
}) {
	return (
		<section {...stylex.props(styles.section)}>
			<header {...stylex.props(styles.header)}>
				<div {...stylex.props(styles.headingGroup)}>
					<h3
						{...stylex.props([
							styles.title,
							tone === "danger" && styles.dangerTitle,
						])}
					>
						{title}
					</h3>
					{lede ? <p {...stylex.props(styles.description)}>{lede}</p> : null}
				</div>
			</header>
			<div {...stylex.props(styles.content)}>{children}</div>
		</section>
	);
}

const styles = stylex.create({
	section: { position: "relative" },
	header: {
		marginBottom: space.lg,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: "color-mix(in oklab, #fff 5%, transparent)",
		paddingBottom: space.md,
	},
	headingGroup: { minWidth: "0rem" },
	title: {
		margin: "0rem",
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
		lineHeight: "1.1",
		letterSpacing: "-0.025em",
		color: colors.ink,
	},
	dangerTitle: { color: colors.failed },
	description: {
		marginTop: "5px",
		marginBottom: "0rem",
		fontSize: "13px",
		lineHeight: "1.45",
		color: colors.muted,
	},
	content: { display: "flex", flexDirection: "column", gap: "0.875rem" },
});
