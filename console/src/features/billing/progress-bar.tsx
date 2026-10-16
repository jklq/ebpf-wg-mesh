import * as stylex from "@stylexjs/stylex";
import { usedFraction } from "#/features/billing/billing-model";
import { colors, fonts, space } from "#/styles/tokens.stylex";

/** A labelled meter of returned cents against a returned limit. */
export function ProgressBar({
	label,
	fraction,
	caption,
}: {
	label: string;
	fraction: { used: string; limit: string };
	caption: string;
}) {
	const value = usedFraction(fraction.used, fraction.limit);
	return (
		<div {...stylex.props(styles.wrapper)}>
			<meter
				aria-label={label}
				aria-valuetext={caption}
				min={0}
				max={1}
				value={value}
				{...stylex.props(styles.hidden)}
			/>
			<div aria-hidden="true" {...stylex.props(styles.track)}>
				<div
					{...stylex.props(
						styles.fill,
						value >= 1 && styles.full,
						styles.width(`${value * 100}%`),
					)}
				/>
			</div>
			<span {...stylex.props(styles.caption)}>{caption}</span>
		</div>
	);
}

const styles = stylex.create({
	wrapper: { display: "flex", flexDirection: "column", gap: space.xs },
	hidden: {
		position: "absolute",
		width: "1px",
		height: "1px",
		overflow: "hidden",
		clipPath: "inset(50%)",
	},
	track: {
		height: "6px",
		overflow: "hidden",
		backgroundColor: colors.surfaceHover,
	},
	fill: { height: "100%", backgroundColor: colors.accent },
	full: { backgroundColor: colors.building },
	width: (width: string) => ({ width }),
	caption: { fontFamily: fonts.mono, fontSize: "11px", color: colors.muted },
});
