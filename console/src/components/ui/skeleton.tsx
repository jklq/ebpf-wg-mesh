import * as stylex from "@stylexjs/stylex";

export function SkeletonShimmer() {
	return <span aria-hidden="true" {...stylex.props(styles.shimmer)} />;
}

export function SkeletonChip({
	styles: overrides,
}: {
	styles?: stylex.StyleXStyles;
}) {
	return (
		<span aria-hidden="true" {...stylex.props(styles.chip, overrides)}>
			<SkeletonShimmer />
		</span>
	);
}

const shimmer = stylex.keyframes({
	from: { transform: "translateX(-120%)" },
	to: { transform: "translateX(120%)" },
});

const styles = stylex.create({
	shimmer: {
		position: "absolute",
		inset: "0rem",
		animation: `${shimmer} 1.25s ease-in-out infinite`,
		backgroundImage:
			"linear-gradient(to right in oklab, transparent 0%, rgba(212,205,197,0.08) 50%, transparent 100%)",
	},
	chip: {
		position: "relative",
		display: "inline-block",
		overflow: "hidden",
		backgroundColor: "rgba(80,76,71,0.42)",
	},
});
