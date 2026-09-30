import * as stylex from "@stylexjs/stylex";
import { colors, motion } from "#/styles/tokens.stylex";

export type ProgressState = "queued" | "running" | "succeeded" | "failed";
export interface ProgressStep {
	id: string;
	label: string;
	state: ProgressState;
}

export function ProgressRail({
	steps,
	label,
	building = false,
	completing = false,
	delay,
	styles,
}: {
	steps: readonly ProgressStep[];
	label: string;
	building?: boolean;
	completing?: boolean;
	delay?: string;
	styles?: stylex.StyleXStyles;
}) {
	return (
		<div
			role="img"
			aria-label={label}
			{...stylex.props(railStyles.rail, styles)}
		>
			{steps.map((step) => (
				<span
					key={step.id}
					title={step.label}
					data-state={completing ? "succeeded" : step.state}
					{...stylex.props(
						railStyles.segment,
						railStyles[completing ? "succeeded" : step.state],
						building &&
							step.state === "succeeded" &&
							!completing &&
							railStyles.buildingComplete,
						delay !== undefined &&
							step.state === "running" &&
							railStyles.delay(delay),
					)}
				/>
			))}
		</div>
	);
}

const pulse = stylex.keyframes({
	"0%, 100%": { opacity: 1 },
	"50%": { opacity: 0.35 },
});
const railStyles = stylex.create({
	rail: {
		display: "grid",
		height: 3,
		width: 54,
		flexShrink: 0,
		gridAutoColumns: "minmax(0, 1fr)",
		gridAutoFlow: "column",
		gap: 2,
	},
	segment: {
		borderRadius: 999,
		transitionProperty: "background-color",
		transitionDuration: motion.normal,
	},
	queued: { backgroundColor: "rgba(80,76,71,0.7)" },
	running: {
		backgroundColor: colors.building,
		animation: `${pulse} 1.4s ease-in-out infinite`,
	},
	succeeded: { backgroundColor: colors.healthy },
	failed: { backgroundColor: colors.failed },
	buildingComplete: { backgroundColor: colors.building },
	delay: (value: string) => ({ animationDelay: value }),
});
