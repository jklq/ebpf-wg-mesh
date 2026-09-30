import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { colors, fonts, motion, space } from "#/styles/tokens.stylex";

export function TabList<Id extends string>({
	items,
	selected,
	onSelect,
	label,
	id,
	panelId,
	styles,
}: {
	items: readonly { id: Id; label: string; icon?: ReactNode }[];
	selected: Id;
	onSelect: (id: Id) => void;
	label: string;
	id: string;
	panelId: string;
	styles?: stylex.StyleXStyles;
}) {
	return (
		<div
			role="tablist"
			aria-label={label}
			{...stylex.props(tabStyles.list, styles)}
			onKeyDown={(event) => {
				const current = items.findIndex((item) => item.id === selected);
				const next =
					event.key === "ArrowRight"
						? (current + 1) % items.length
						: event.key === "ArrowLeft"
							? (current + items.length - 1) % items.length
							: event.key === "Home"
								? 0
								: event.key === "End"
									? items.length - 1
									: -1;
				if (next < 0 || !items[next]) return;
				event.preventDefault();
				onSelect(items[next].id);
				event.currentTarget
					.querySelectorAll<HTMLButtonElement>('[role="tab"]')
					[next]?.focus();
			}}
		>
			{items.map((item) => (
				<button
					key={item.id}
					id={`${id}-${item.id}`}
					type="button"
					role="tab"
					aria-selected={selected === item.id}
					aria-controls={panelId}
					tabIndex={selected === item.id ? 0 : -1}
					onClick={() => onSelect(item.id)}
					{...stylex.props(
						tabStyles.tab,
						selected === item.id ? tabStyles.selected : tabStyles.idle,
					)}
				>
					{item.icon}
					<span>{item.label}</span>
				</button>
			))}
		</div>
	);
}

const tabStyles = stylex.create({
	list: {
		display: "flex",
		minHeight: 54,
		flexShrink: 0,
		alignItems: "flex-end",
		gap: 22,
		overflowX: "auto",
		borderBottomWidth: 1,
		borderBottomStyle: "solid",
		borderColor: colors.line,
		backgroundImage:
			"linear-gradient(180deg,rgba(255,255,255,0.035),transparent)",
		backgroundColor: "rgba(20,18,16,0.88)",
		paddingInline: space.lg,
		paddingTop: 10,
	},
	tab: {
		display: "inline-flex",
		height: 34,
		flexShrink: 0,
		cursor: "pointer",
		alignItems: "center",
		gap: 6,
		border: 0,
		borderBottomWidth: 2,
		borderBottomStyle: "solid",
		backgroundColor: "transparent",
		paddingInline: 2,
		paddingBottom: 10,
		fontFamily: fonts.condensed,
		fontSize: 12,
		fontWeight: 700,
		letterSpacing: "0.12em",
		whiteSpace: "nowrap",
		textTransform: "uppercase",
		transitionProperty: "color,border-color",
		transitionDuration: motion.fast,
	},
	selected: { borderColor: colors.accent, color: colors.accent },
	idle: {
		borderColor: "transparent",
		color: { default: colors.dim, ":hover": colors.ink },
	},
});
