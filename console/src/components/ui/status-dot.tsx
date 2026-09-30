import * as stylex from "@stylexjs/stylex";
import type { ComponentProps } from "react";
import { colors, shape } from "#/styles/tokens.stylex";

const pulseBuilding = stylex.keyframes({
	"0%, 100%": { opacity: 1 },
	"50%": { opacity: 0.35 },
});
export type HealthTone = "healthy" | "building" | "failed" | "offline";
export function statusDotStylesFor(health: HealthTone): stylex.StyleXStyles {
	return [statusDotStyles.base, statusDotStyles[health]];
}
export function StatusDot({
	health,
	delay,
	styles,
	...props
}: Omit<ComponentProps<"span">, "className" | "style"> & {
	health: HealthTone;
	delay?: string;
	styles?: stylex.StyleXStyles;
}) {
	return (
		<span
			{...props}
			data-health={health}
			aria-hidden={props["aria-label"] ? undefined : true}
			{...stylex.props(
				statusDotStylesFor(health),
				delay ? delayStyles.delay(delay) : undefined,
				styles,
			)}
		/>
	);
}
const delayStyles = stylex.create({
	delay: (value: string) => ({ animationDelay: value }),
});

export const statusDotStyles = stylex.create({
	base: {
		width: "7px",
		height: "7px",
		flexShrink: "0",
		borderRadius: shape.control,
	},
	building: {
		backgroundColor: colors.building,
		animation: `${pulseBuilding} 1.4s ease-in-out infinite`,
	},
	failed: { backgroundColor: colors.failed },
	offline: { backgroundColor: colors.offline },
	healthy: { backgroundColor: colors.healthy },
});
