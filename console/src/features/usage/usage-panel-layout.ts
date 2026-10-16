import * as stylex from "@stylexjs/stylex";
import { colors, shape, space } from "#/styles/tokens.stylex";

/**
 * Box geometry of the service usage panel and its charts, shared with the
 * loading skeleton so the two keep the same shape.
 */
export const usagePanelLayout = stylex.create({
	toolbar: { display: "flex", alignItems: "center", gap: space.sm },
	cardsContainer: { containerType: "inline-size" },
	cards: { display: "grid", gridTemplateColumns: "1fr", gap: space.md },
	grid: {
		gridTemplateColumns: {
			default: "1fr",
			"@container (width >= 36rem)": "repeat(2, minmax(0, 1fr))",
			"@container (width >= 60rem)": "repeat(3, minmax(0, 1fr))",
		},
	},
	card: {
		display: "flex",
		minWidth: "0rem",
		flexDirection: "column",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.card,
		backgroundColor: colors.surface,
		paddingInline: space.lg,
		paddingBlock: space.md,
	},
	cardHeader: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "baseline",
		columnGap: space.md,
		rowGap: space.xs,
	},
	figure: {
		margin: "0rem",
		display: "grid",
		gridTemplateColumns: "auto 1fr",
		columnGap: space.sm,
		rowGap: space.xs,
	},
	// A fixed width lines stacked charts up so their crosshairs align.
	valueAxis: {
		display: "flex",
		width: "3.5rem",
		height: "150px",
		flexDirection: "column",
		justifyContent: "space-between",
		alignItems: "flex-end",
	},
	plot: { position: "relative", height: "150px", minWidth: "0rem" },
	timeAxis: { position: "relative", height: "14px" },
});
