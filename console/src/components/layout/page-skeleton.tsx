import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { PageShell } from "#/components/layout/page-shell";
import { SkeletonChip } from "#/components/ui/skeleton";
import { colors, shape, space } from "#/styles/tokens.stylex";

export function PageSkeleton({
	title,
	icon,
}: {
	title: string;
	icon: ReactNode;
}) {
	return (
		<PageShell title={title} icon={icon}>
			<output
				aria-label={`Loading ${title.toLowerCase()}`}
				{...stylex.props(styles.hidden)}
			>
				Loading {title.toLowerCase()}…
			</output>
			<div aria-hidden="true" {...stylex.props(styles.content)}>
				<SkeletonChip styles={styles.title} />
				{[0, 1, 2].map((index) => (
					<div key={index} {...stylex.props(styles.card)}>
						<SkeletonChip styles={styles.line} />
						<SkeletonChip styles={styles.field} />
					</div>
				))}
			</div>
		</PageShell>
	);
}

const styles = stylex.create({
	hidden: {
		position: "absolute",
		width: 1,
		height: 1,
		overflow: "hidden",
		clipPath: "inset(50%)",
		whiteSpace: "nowrap",
	},
	content: { display: "flex", flexDirection: "column", gap: space.xl },
	title: { height: 32, width: 220, maxWidth: "100%" },
	card: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		padding: space.xl,
		borderWidth: 1,
		borderStyle: "solid",
		borderColor: colors.line,
		borderRadius: shape.card,
		backgroundColor: colors.surface,
	},
	line: { height: 16, width: "35%" },
	field: { height: 36, width: "70%" },
});
