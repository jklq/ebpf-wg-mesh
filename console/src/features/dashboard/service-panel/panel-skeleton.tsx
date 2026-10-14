import * as stylex from "@stylexjs/stylex";
import { SkeletonChip } from "#/components/ui/skeleton";
import { colors, shape, space } from "#/styles/tokens.stylex";

function LoadingLabel({ label }: { label: string }) {
	return <output {...stylex.props(styles.hidden)}>{label}</output>;
}

/** Generic placeholder for a service panel tab whose content is still loading. */
export function PanelTabSkeleton() {
	return (
		<>
			<LoadingLabel label="Loading…" />
			<div aria-hidden="true" {...stylex.props(styles.tab)}>
				{[0, 1].map((index) => (
					<div key={index} {...stylex.props(styles.section)}>
						<SkeletonChip styles={styles.sectionTitle} />
						<SkeletonChip styles={styles.sectionLede} />
						<PanelRowSkeleton />
					</div>
				))}
			</div>
		</>
	);
}

/** Placeholder for a single bordered row, such as a domain binding. */
export function PanelRowSkeleton() {
	return (
		<div aria-hidden="true" {...stylex.props(styles.row)}>
			<SkeletonChip styles={styles.rowIcon} />
			<SkeletonChip styles={styles.rowText} />
		</div>
	);
}

/** Placeholder shaped like a live deployment card. */
export function DeploymentCardSkeleton() {
	return (
		<>
			<LoadingLabel label="Loading deployments…" />
			<div aria-hidden="true" {...stylex.props(styles.deploymentCard)}>
				<SkeletonChip styles={styles.badge} />
				<SkeletonChip styles={styles.avatar} />
				<div {...stylex.props(styles.deploymentCopy)}>
					<SkeletonChip styles={styles.deploymentTitle} />
					<SkeletonChip styles={styles.deploymentMeta} />
				</div>
				<SkeletonChip styles={styles.button} />
			</div>
		</>
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
	tab: { display: "flex", flexDirection: "column", gap: space.xl },
	section: { display: "flex", flexDirection: "column", gap: space.sm },
	sectionTitle: { height: 14, width: 120 },
	sectionLede: { height: 12, width: "55%", marginBottom: space.xs },
	row: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		paddingInline: "0.875rem",
		paddingBlock: space.md,
	},
	rowIcon: { height: 12, width: 12 },
	rowText: { height: 13, width: "45%" },
	deploymentCard: {
		display: "flex",
		alignItems: "center",
		gap: space.lg,
		flexShrink: "0",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "rgba(25,24,22,0.96)",
		paddingInline: { default: "1.75rem", "@media (width < 900px)": "1rem" },
		paddingBlock: "1.75rem",
	},
	badge: { height: 26, width: 92, flexShrink: "0" },
	avatar: { height: 40, width: 40, flexShrink: "0", borderRadius: "50%" },
	deploymentCopy: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		flexDirection: "column",
		gap: space.sm,
	},
	deploymentTitle: { height: 18, width: "60%" },
	deploymentMeta: { height: 12, width: "35%" },
	button: { height: 36, width: 120, flexShrink: "0" },
});
