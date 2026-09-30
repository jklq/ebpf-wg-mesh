import * as stylex from "@stylexjs/stylex";
import type { ComponentProps } from "react";
import { colors, fonts, motion, space } from "#/styles/tokens.stylex";

type InputProps = Omit<ComponentProps<"input">, "className" | "style"> & {
	unapplied?: boolean;
	styles?: stylex.StyleXStyles;
	"data-unapplied"?: boolean;
};
export function TextInput({ unapplied = false, styles, ...props }: InputProps) {
	return (
		<input
			{...props}
			data-unapplied={unapplied || props["data-unapplied"]}
			{...stylex.props(
				fieldStyles.input,
				unapplied && fieldStyles.unapplied,
				styles,
			)}
		/>
	);
}

export const fieldStyles = stylex.create({
	label: {
		marginBottom: "0.375rem",
		display: "block",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.11em",
		color: colors.label,
	},
	input: {
		width: "100%",
		appearance: "none",
		borderRadius: "0",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: { default: colors.line, ":focus": colors.accent },
		backgroundColor: colors.canvas,
		paddingInline: "0.625rem",
		paddingBlock: space.sm,
		fontFamily: fonts.mono,
		fontSize: "13px",
		color: colors.ink,
		outlineStyle: "none",
		transitionProperty: "border-color",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
		"::placeholder": { color: colors.dim },
		boxShadow: { default: null, ":focus": `0 0 0 2px ${colors.accentDim}` },
	},
	unapplied: {
		borderColor: {
			default: "rgba(139,184,236,0.72)",
			":focus": colors.unapplied,
		},
		backgroundColor: "rgba(74,121,178,0.11)",
		boxShadow: {
			default: "inset 0 0 0 1px rgba(139,184,236,0.18)",
			":focus":
				"inset 0 0 0 1px rgba(139,184,236,0.24), 0 0 0 2px rgba(139,184,236,0.18)",
		},
	},
	unappliedSurface: {
		borderColor: "rgba(139,184,236,0.72)",
		backgroundColor: "rgba(74,121,178,0.11)",
	},
});
