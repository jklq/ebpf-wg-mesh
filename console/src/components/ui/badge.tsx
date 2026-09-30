import * as stylex from "@stylexjs/stylex";
import type { ComponentProps } from "react";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";
export type BadgeTone =
	| "healthy"
	| "building"
	| "failed"
	| "offline"
	| "edited";
export function badgeStylesFor(tone: BadgeTone): stylex.StyleXStyles {
	return [badgeStyles.base, badgeStyles[tone]];
}
export function Badge({
	tone,
	styles,
	...props
}: Omit<ComponentProps<"span">, "className" | "style"> & {
	tone: BadgeTone;
	styles?: stylex.StyleXStyles;
}) {
	return <span {...props} {...stylex.props(badgeStylesFor(tone), styles)} />;
}

export const badgeStyles = stylex.create({
	base: {
		display: "inline-flex",
		alignItems: "center",
		gap: space.xs,
		borderRadius: shape.control,
		paddingInline: "0.375rem",
		paddingBlock: "0.125rem",
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.09em",
	},
	healthy: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(92,170,112,0.22)",
		backgroundColor: colors.healthyDim,
		color: colors.healthy,
	},
	building: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(192,133,32,0.22)",
		backgroundColor: colors.buildingDim,
		color: colors.building,
	},
	failed: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(184,66,66,0.22)",
		backgroundColor: colors.failedDim,
		color: colors.failed,
	},
	offline: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "rgba(44,42,38,0.5)",
		color: colors.muted,
	},
	edited: {
		whiteSpace: "nowrap",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(139,184,236,0.28)",
		backgroundColor: "rgba(74,121,178,0.14)",
		color: colors.unapplied,
	},
});
