import * as stylex from "@stylexjs/stylex";
import { SkeletonChip } from "#/components/ui/skeleton";
import { WorkspacePageHeader } from "#/features/workspace/page-header";
import type { WorkspaceSection } from "#/features/workspace/workspace-shell";
import { colors, shape, space } from "#/styles/tokens.stylex";

export function WorkspacePageSkeleton({
	section,
}: {
	section: WorkspaceSection;
}) {
	const title =
		section === "projects"
			? "Projects"
			: section === "people"
				? "People"
				: "Settings";
	return (
		<section
			aria-label={`Loading ${title.toLowerCase()}`}
			aria-busy="true"
			{...stylex.props(styles.content)}
		>
			<output
				aria-label={`Loading ${title.toLowerCase()}`}
				{...stylex.props(styles.hidden)}
			>
				Loading {title.toLowerCase()}…
			</output>
			<WorkspacePageHeader
				title={title}
				lede={<SkeletonChip styles={styles.lede} />}
			/>
			<div
				aria-hidden="true"
				{...stylex.props(section === "projects" ? styles.grid : styles.rows)}
			>
				{[0, 1, 2].map((index) => (
					<div
						key={index}
						{...stylex.props(
							styles.card,
							section === "people" && styles.person,
						)}
					>
						<SkeletonChip styles={styles.title} />
						<SkeletonChip
							styles={section === "projects" ? styles.preview : styles.line}
						/>
						<SkeletonChip styles={styles.line} />
					</div>
				))}
			</div>
		</section>
	);
}

export function WorkspaceShellSkeleton({
	section,
}: {
	section: WorkspaceSection;
}) {
	return (
		<div {...stylex.props(styles.layout)}>
			<aside aria-hidden="true" {...stylex.props(styles.sidebar)}>
				<SkeletonChip styles={styles.title} />
				<SkeletonChip styles={styles.switcher} />
				{[0, 1, 2].map((index) => (
					<SkeletonChip key={index} styles={styles.nav} />
				))}
			</aside>
			<main {...stylex.props(styles.main)}>
				<WorkspacePageSkeleton section={section} />
			</main>
		</div>
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
	content: {
		display: "flex",
		flexDirection: "column",
		gap: space.xl,
		minWidth: 0,
	},
	lede: { height: 13, width: 220, maxWidth: "100%" },
	grid: {
		display: "grid",
		gridTemplateColumns: "repeat(auto-fill, minmax(17.5rem, 1fr))",
		gap: space.lg,
	},
	rows: { display: "flex", flexDirection: "column", gap: space.lg },
	card: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		padding: space.lg,
		borderRadius: shape.card,
		borderWidth: 1,
		borderStyle: "solid",
		borderColor: colors.line,
		backgroundColor: colors.surface,
	},
	person: { flexDirection: "row", alignItems: "center", minHeight: 64 },
	title: { height: 22, width: 140, maxWidth: "100%" },
	line: { height: 13, width: "35%" },
	preview: { height: 120, width: "100%", borderRadius: shape.card },
	layout: {
		display: "flex",
		minHeight: "100dvh",
		flexDirection: { default: "row", "@media (max-width: 767px)": "column" },
		backgroundColor: colors.canvas,
	},
	sidebar: {
		display: "flex",
		flexDirection: "column",
		flexShrink: 0,
		gap: space.lg,
		width: { default: 240, "@media (max-width: 767px)": "100%" },
		padding: space.lg,
		borderRightWidth: 1,
		borderRightStyle: "solid",
		borderColor: colors.line,
		backgroundColor: colors.surface,
	},
	switcher: { width: "100%", height: 58 },
	nav: { width: "100%", height: 32 },
	main: {
		minWidth: 0,
		flex: 1,
		paddingInline: { default: "3rem", "@media (max-width: 767px)": space.lg },
		paddingBlock: { default: "3rem", "@media (max-width: 767px)": space.xl },
	},
});
