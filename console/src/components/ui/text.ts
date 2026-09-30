import * as stylex from "@stylexjs/stylex";
import { colors, fonts } from "#/styles/tokens.stylex";

export const textStyles = stylex.create({
	eyebrow: {
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.muted,
	},
});
