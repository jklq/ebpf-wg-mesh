import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	dangerEmphasis: { color: colors.failed },
	settingsCard: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		borderStyle: "solid",
		borderWidth: "1px",
		backgroundColor: colors.surface,
		padding: "1.25rem",
	},
	dangerCard: { borderColor: "rgba(184,66,66,0.4)" },
	defaultCard: { borderColor: colors.line },
	cardTitle: {
		margin: "0rem",
		fontFamily: fonts.display,
		fontSize: "20px",
		fontWeight: "500",
		letterSpacing: "-0.025em",
	},
	defaultCardTitle: { color: colors.ink },
	cardDescription: {
		marginTop: space.xs,
		marginBottom: "0rem",
		fontSize: "13px",
		lineHeight: "1.45",
		color: colors.muted,
	},
});
export function SettingsCard({
	title,
	lede,
	tone = "default",
	children,
}: {
	title: string;
	lede?: string;
	tone?: "default" | "danger";
	children: ReactNode;
}) {
	return (
		<section
			{...stylex.props([
				styles.settingsCard,
				tone === "danger" ? styles.dangerCard : styles.defaultCard,
			])}
		>
			<header>
				<h2
					{...stylex.props([
						styles.cardTitle,
						tone === "danger" ? styles.dangerEmphasis : styles.defaultCardTitle,
					])}
				>
					{title}
				</h2>
				{lede && <p {...stylex.props(styles.cardDescription)}>{lede}</p>}
			</header>
			{children}
		</section>
	);
}
