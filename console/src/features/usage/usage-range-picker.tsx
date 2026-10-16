import * as stylex from "@stylexjs/stylex";
import { Clock } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { textStyles } from "#/components/ui/text";
import { type UsageRange, usageRanges } from "#/features/usage/usage-model";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

/** A "Last …" button that opens a grid of quick ranges. */
export function UsageRangePicker({
	ranges,
	selected,
	onSelect,
}: {
	ranges: readonly UsageRange[];
	selected: UsageRange;
	onSelect: (range: UsageRange) => void;
}) {
	const [open, setOpen] = useState(false);
	const rootRef = useRef<HTMLDivElement>(null);

	useEffect(() => {
		if (!open) return;
		const onPointerDown = (event: MouseEvent) => {
			if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
		};
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key === "Escape") setOpen(false);
		};
		document.addEventListener("mousedown", onPointerDown);
		document.addEventListener("keydown", onKeyDown);
		return () => {
			document.removeEventListener("mousedown", onPointerDown);
			document.removeEventListener("keydown", onKeyDown);
		};
	}, [open]);

	return (
		<div ref={rootRef} {...stylex.props(styles.root)}>
			<button
				type="button"
				aria-haspopup="true"
				aria-expanded={open}
				onClick={() => setOpen((current) => !current)}
				{...stylex.props(
					usageControlStyles.control,
					open && styles.openControl,
				)}
			>
				<Clock size={13} />
				Last {usageRanges[selected].label}
			</button>
			{open && (
				<div {...stylex.props(styles.popover)}>
					<p {...stylex.props(textStyles.eyebrow, styles.heading)}>
						Quick range
					</p>
					<div {...stylex.props(styles.options)}>
						{ranges.map((range) => (
							<button
								key={range}
								type="button"
								aria-pressed={range === selected}
								onClick={() => {
									onSelect(range);
									setOpen(false);
								}}
								{...stylex.props(
									styles.option,
									range === selected && styles.selectedOption,
								)}
							>
								{usageRanges[range].label}
							</button>
						))}
					</div>
				</div>
			)}
		</div>
	);
}

/** Shared look for the panel's toolbar controls. */
export const usageControlStyles = stylex.create({
	control: {
		display: "inline-flex",
		height: "30px",
		cursor: "pointer",
		alignItems: "center",
		gap: "6px",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
		},
		borderRadius: shape.card,
		backgroundColor: colors.surfaceRaised,
		paddingInline: "0.625rem",
		fontFamily: fonts.sans,
		fontSize: "12px",
		color: colors.ink,
		transitionProperty: "border-color, color",
		transitionDuration: motion.fast,
	},
});

const styles = stylex.create({
	root: { position: "relative" },
	openControl: { borderColor: colors.lineBright },
	popover: {
		position: "absolute",
		top: "calc(100% + 6px)",
		right: "0rem",
		zIndex: 20,
		width: "17rem",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		borderRadius: shape.card,
		backgroundColor: colors.surface,
		padding: space.md,
		boxShadow: "0 18px 50px rgba(0,0,0,0.55)",
	},
	heading: { marginTop: "0rem", marginBottom: space.sm },
	options: {
		display: "grid",
		gridTemplateColumns: "repeat(3, 1fr)",
		gap: "6px",
	},
	option: {
		cursor: "pointer",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.control,
		backgroundColor: {
			default: "transparent",
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		paddingBlock: "6px",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.label,
	},
	selectedOption: {
		borderColor: colors.accentGlow,
		backgroundColor: colors.accentDim,
		color: colors.accent,
	},
});
