import * as stylex from "@stylexjs/stylex";
import type { ComponentProps } from "react";
import { colors, fonts, space } from "#/styles/tokens.stylex";
export function Notice({
	tone = "error",
	styles,
	...props
}: Omit<ComponentProps<"div">, "className" | "style"> & {
	tone?: "error" | "success";
	styles?: stylex.StyleXStyles;
}) {
	return (
		<div
			{...props}
			role={props.role ?? (tone === "error" ? "alert" : "status")}
			{...stylex.props(noticeStyles[tone], styles)}
		/>
	);
}

export const noticeStyles = stylex.create({
	error: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(184,66,66,0.25)",
		borderLeftStyle: "solid",
		borderLeftWidth: "3px",
		borderLeftColor: colors.failed,
		backgroundColor: colors.failedDim,
		paddingInline: "0.625rem",
		paddingBlock: space.sm,
		fontFamily: fonts.mono,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.failed,
	},
	success: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(109,190,130,0.32)",
		borderLeftStyle: "solid",
		borderLeftWidth: "3px",
		borderLeftColor: colors.healthy,
		backgroundColor: colors.healthyDim,
		paddingInline: "0.625rem",
		paddingBlock: space.sm,
		fontSize: "13px",
		color: "#9ed6ab",
	},
});
