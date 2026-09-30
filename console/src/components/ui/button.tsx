import * as stylex from "@stylexjs/stylex";
import type { ComponentProps } from "react";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

export type ButtonVariant =
	| "primary"
	| "secondary"
	| "ghost"
	| "danger"
	| "dangerSolid"
	| "dangerOutline"
	| "icon"
	| "panelIcon";
type ButtonProps = Omit<ComponentProps<"button">, "className" | "style"> & {
	variant?: ButtonVariant;
	styles?: stylex.StyleXStyles;
};

export function Button({
	variant = "secondary",
	styles,
	type = "button",
	...props
}: ButtonProps) {
	return (
		<button
			{...props}
			type={type}
			aria-label={
				props["aria-label"] ??
				(variant === "icon" || variant === "panelIcon"
					? props.title
					: undefined)
			}
			{...stylex.props(buttonStyles.base, buttonStyles[variant], styles)}
		/>
	);
}

// Export the same recipes for native links and router links.
export const buttonStyles = stylex.create({
	base: {
		display: "inline-flex",
		alignItems: "center",
		justifyContent: "center",
		gap: 6,
		cursor: { default: "pointer", ":disabled": "not-allowed" },
		borderRadius: shape.control,
		borderWidth: 1,
		borderStyle: "solid",
		borderColor: colors.line,
		paddingInline: 14,
		paddingBlock: 7,
		fontFamily: fonts.condensed,
		fontSize: 11,
		fontWeight: 700,
		textTransform: "uppercase",
		letterSpacing: "0.07em",
		textDecoration: "none",
		transitionProperty:
			"color, background-color, border-color, filter, transform",
		transitionDuration: motion.fast,
		transitionTimingFunction: motion.ease,
		opacity: { default: 1, ":disabled": 0.35 },
	},
	primary: {
		backgroundColor: colors.accent,
		borderWidth: 0,
		color: "#0f0e0d",
		letterSpacing: "0.09em",
		transitionProperty: "filter, transform",
		filter: { default: "none", ":enabled:hover": "brightness(1.1)" },
		transform: { default: "none", ":enabled:active": "translateY(1px)" },
	},
	secondary: {
		fontWeight: 600,
		transitionProperty: "border-color, background-color",
		backgroundColor: {
			default: "transparent",
			":enabled:hover": colors.surfaceHover,
		},
		borderColor: { default: colors.line, ":enabled:hover": colors.lineBright },
		color: colors.ink,
	},
	ghost: {
		borderWidth: 0,
		transitionProperty: "background-color, color",
		backgroundColor: {
			default: "transparent",
			":enabled:hover": colors.surfaceHover,
		},
		paddingInline: space.sm,
		paddingBlock: 6,
		fontFamily: fonts.sans,
		fontSize: 11,
		fontWeight: 500,
		textTransform: "none",
		letterSpacing: "normal",
		color: { default: colors.muted, ":enabled:hover": colors.ink },
	},
	danger: {
		borderColor: colors.failed,
		backgroundColor: {
			default: colors.failedDim,
			":enabled:hover": "rgba(184,66,66,0.22)",
		},
		color: colors.failed,
	},
	dangerSolid: {
		borderColor: { default: colors.failed, ":disabled": "rgba(184,66,66,0.4)" },
		backgroundColor: {
			default: colors.failed,
			":disabled": "rgba(184,66,66,0.35)",
		},
		color: { default: "#f5eceb", ":disabled": "rgba(245,236,235,0.55)" },
		opacity: 1,
		letterSpacing: "0.09em",
		transitionProperty: "filter, transform",
		transform: { default: "none", ":enabled:active": "translateY(1px)" },
		paddingInline: 18,
		paddingBlock: space.sm,
		filter: { default: "none", ":enabled:hover": "brightness(1.1)" },
	},
	dangerOutline: {
		transitionProperty: "background-color, border-color",
		borderColor: {
			default: "rgba(184,66,66,0.5)",
			":enabled:hover": colors.failed,
		},
		backgroundColor: {
			default: "transparent",
			":enabled:hover": colors.failedDim,
		},
		color: colors.failed,
		whiteSpace: "nowrap",
	},
	icon: {
		width: 30,
		height: 30,
		transitionProperty: "none",
		padding: 0,
		backgroundColor: {
			default: "transparent",
			":enabled:hover": colors.surfaceHover,
		},
		borderColor: { default: colors.line, ":enabled:hover": colors.lineBright },
		color: { default: colors.muted, ":enabled:hover": colors.ink },
	},
	panelIcon: {
		width: 32,
		height: 32,
		borderRadius: 0,
		transitionProperty: "color, background-color, border-color",
		opacity: { default: 1, ":disabled": 0.45 },
		padding: 0,
		backgroundColor: {
			default: "transparent",
			":enabled:hover": colors.surfaceHover,
		},
		borderColor: { default: "transparent", ":enabled:hover": colors.line },
		color: { default: colors.muted, ":enabled:hover": colors.ink },
	},
});
