import * as stylex from "@stylexjs/stylex";
import { NODE_H, NODE_W } from "#/features/dashboard/canvas/layout";
import { colors, shape, sizes, space } from "#/styles/tokens.stylex";

const skeletonShimmer = stylex.keyframes({
	"100%": { transform: "translateX(120%)" },
});

const styles = stylex.create({
	dimensions: (width: number, height: number) => ({ width, height }),
	shimmer: {
		position: "absolute",
		inset: "0rem",
		translate: "-120% 0",
		animation: `${skeletonShimmer} 1.25s ease-in-out infinite`,
		backgroundImage:
			"linear-gradient(to right in oklab, transparent 0%, rgba(212,205,197,0.08) 50%, transparent 100%)",
	},
	chip: {
		position: "relative",
		display: "inline-block",
		overflow: "hidden",
		backgroundColor: "rgba(80,76,71,0.42)",
	},
	page: {
		display: "flex",
		height: "100dvh",
		flexDirection: "column",
		overflow: "hidden",
		backgroundColor: colors.canvas,
	},
	toolbar: {
		pointerEvents: "none",
		position: "fixed",
		insetInline: "0rem",
		top: "0rem",
		zIndex: "40",
		display: "flex",
		height: sizes.header,
		alignItems: "center",
		gap: space.md,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		paddingInline: space.lg,
	},
	brandMark: {
		position: "relative",
		width: "15px",
		height: "15px",
		flexShrink: "0",
		overflow: "hidden",
		backgroundColor: "rgba(212,119,26,0.22)",
		clipPath: "polygon(52% 0,100% 44%,68% 44%,86% 100%,0 48%,40% 48%)",
	},
	brandChip: { height: "0.875rem", width: "3rem" },
	projectChip: {
		height: "1.5rem",
		width: "108px",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
	},
	environmentChip: { height: "0.625rem", width: "66px" },
	toolbarSpacer: { flex: "1" },
	toolbarActionChip: {
		width: "1.75rem",
		height: "1.75rem",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
	},
	deployActionChip: {
		height: "30px",
		width: "6rem",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(212,119,26,0.24)",
		backgroundColor: "rgba(212,119,26,0.08)",
	},
	canvas: { overflow: "hidden", backgroundColor: colors.canvas },
	standaloneCanvas: {
		position: "relative",
		marginTop: sizes.header,
		height: `calc(100dvh - ${sizes.header})`,
	},
	overlayCanvas: {
		pointerEvents: "none",
		position: "absolute",
		inset: "0rem",
		zIndex: "15",
	},
	worldGrid: {
		position: "absolute",
		left: "-8192px",
		top: "-8192px",
		width: "16384px",
		height: "16384px",
		pointerEvents: "none",
		backgroundImage:
			"linear-gradient(rgba(54, 50, 47, 0.65) 1px, transparent 1px), linear-gradient(90deg, rgba(54, 50, 47, 0.65) 1px, transparent 1px), linear-gradient(rgba(38, 36, 33, 0.38) 1px, transparent 1px), linear-gradient(90deg, rgba(38, 36, 33, 0.38) 1px, transparent 1px)",
		backgroundSize: "128px 128px, 128px 128px, 32px 32px, 32px 32px",
	},
	world: { pointerEvents: "none", position: "absolute", inset: "0rem" },
	skeletonNode: {
		left: {
			default: "calc(round(down, calc(50% - 128px), 128px))",
			"@supports not (left: round(down, 1px, 1px))": "calc(50% - 128px)",
		},
		top: {
			default: "calc(round(down, calc(50% - 64px), 128px))",
			"@supports not (left: round(down, 1px, 1px))": "calc(50% - 64px)",
		},
		pointerEvents: "none",
		position: "absolute",
		display: "flex",
		flexDirection: "column",
		overflow: "hidden",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		opacity: "0.92",
	},
	nodeHeader: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: "0.875rem",
		paddingTop: "0.625rem",
		paddingBottom: space.sm,
	},
	statusChip: {
		position: "relative",
		width: "7px",
		height: "7px",
		flexShrink: "0",
		overflow: "hidden",
		borderRadius: shape.card,
		backgroundColor: "rgba(212,119,26,0.22)",
	},
	serviceNameChip: { height: "13px", width: "104px" },
	nodeContent: {
		display: "flex",
		flex: "1",
		flexDirection: "column",
		gap: "7px",
		paddingInline: "0.875rem",
		paddingTop: space.sm,
		paddingBottom: "0.625rem",
	},
	metadataRow: { display: "flex", alignItems: "center", gap: "5px" },
	metadataIconChip: { width: "0.625rem", height: "0.625rem", flexShrink: "0" },
	repositoryChip: { height: "9px", width: "86px" },
	trackedRefChip: { height: "9px", width: "58px" },
	nodeFooter: {
		marginTop: "auto",
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: "0.375rem",
	},
	commitChip: { height: "0.625rem", width: "42px" },
});
export function SkeletonShimmer() {
	return <span {...stylex.props(styles.shimmer)} />;
}
export function SkeletonChip({
	styles: overrides,
}: {
	styles?: stylex.StyleXStyles;
}) {
	return (
		<span {...stylex.props([styles.chip, overrides])}>
			<SkeletonShimmer />
		</span>
	);
}
export function DashboardCanvasSkeleton({
	showTopbar = false,
}: {
	showTopbar?: boolean;
}) {
	return (
		<div
			{...stylex.props(showTopbar ? styles.page : undefined)}
			aria-hidden="true"
		>
			{showTopbar && (
				<div {...stylex.props(styles.toolbar)}>
					<span {...stylex.props(styles.brandMark)}>
						<SkeletonShimmer />
					</span>
					<SkeletonChip styles={styles.brandChip} />
					<SkeletonChip styles={styles.projectChip} />
					<SkeletonChip styles={styles.environmentChip} />
					<span {...stylex.props(styles.toolbarSpacer)} />
					<SkeletonChip styles={styles.toolbarActionChip} />
					<SkeletonChip styles={styles.deployActionChip} />
					<SkeletonChip styles={styles.toolbarActionChip} />
				</div>
			)}
			<div
				{...stylex.props([
					styles.canvas,
					showTopbar ? styles.standaloneCanvas : styles.overlayCanvas,
				])}
			>
				<div {...stylex.props(styles.worldGrid)} />
				<div {...stylex.props(styles.world)}>
					<SkeletonNode />
				</div>
			</div>
		</div>
	);
}
export function SkeletonNode() {
	return (
		<div
			{...stylex.props(styles.skeletonNode, styles.dimensions(NODE_W, NODE_H))}
		>
			<SkeletonShimmer />
			<div {...stylex.props(styles.nodeHeader)}>
				<span {...stylex.props(styles.statusChip)}>
					<SkeletonShimmer />
				</span>
				<SkeletonChip styles={styles.serviceNameChip} />
			</div>
			<div {...stylex.props(styles.nodeContent)}>
				<div {...stylex.props(styles.metadataRow)}>
					<SkeletonChip styles={styles.metadataIconChip} />
					<SkeletonChip styles={styles.repositoryChip} />
				</div>
				<div {...stylex.props(styles.metadataRow)}>
					<SkeletonChip styles={styles.metadataIconChip} />
					<SkeletonChip styles={styles.trackedRefChip} />
				</div>
				<div {...stylex.props(styles.nodeFooter)}>
					<span />
					<SkeletonChip styles={styles.commitChip} />
				</div>
			</div>
		</div>
	);
}
