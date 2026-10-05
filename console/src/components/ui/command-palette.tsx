import * as stylex from "@stylexjs/stylex";
import { ChevronRight, Loader2 } from "lucide-react";
import {
	type ComponentType,
	type KeyboardEvent,
	type ReactNode,
	useEffect,
	useRef,
	useState,
} from "react";
import { noticeStyles } from "#/components/ui/notice";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

export type PaletteItem = {
	id: string;
	label: string;
	icon?: ComponentType<{ size?: number }>;
	/** Shows a chevron: picking the item opens another step. */
	next?: boolean;
	/** Renders the item as a link instead of calling onPick. */
	href?: string;
};

/** Breadcrumb chips for the steps taken so far. */
export function PaletteCrumbs({ crumbs }: { crumbs: Array<string> }) {
	if (crumbs.length === 0) return null;
	return (
		<div {...stylex.props(styles.crumbs)}>
			{crumbs.map((crumb) => (
				<span key={crumb} {...stylex.props(styles.crumb)}>
					{crumb}
				</span>
			))}
		</div>
	);
}

/** A filterable, keyboard-driven list of choices. */
export function PaletteList({
	crumbs = [],
	placeholder,
	items,
	emptyText,
	onPick,
	onBack,
}: {
	crumbs?: Array<string>;
	placeholder: string;
	items: Array<PaletteItem>;
	emptyText: string;
	onPick: (item: PaletteItem) => void;
	onBack?: () => void;
}) {
	const [query, setQuery] = useState("");
	const [highlighted, setHighlighted] = useState(0);
	const filtered = items.filter((item) =>
		item.label.toLowerCase().includes(query.trim().toLowerCase()),
	);
	const listRef = useRef<HTMLDivElement>(null);

	useEffect(() => {
		listRef.current
			?.querySelector<HTMLElement>(`[data-palette-index="${highlighted}"]`)
			?.scrollIntoView?.({ block: "nearest" });
	}, [highlighted]);

	const activate = (item: PaletteItem | undefined) => {
		if (!item) return;
		if (item.href) window.location.href = item.href;
		else onPick(item);
	};

	return (
		<>
			<PaletteCrumbs crumbs={crumbs} />
			<PaletteInput
				placeholder={placeholder}
				value={query}
				onChange={(value) => {
					setQuery(value);
					setHighlighted(0);
				}}
				onBack={onBack}
				onKeyDown={(event) => {
					if (event.key === "ArrowDown") {
						event.preventDefault();
						setHighlighted((index) =>
							Math.min(index + 1, Math.max(filtered.length - 1, 0)),
						);
					} else if (event.key === "ArrowUp") {
						event.preventDefault();
						setHighlighted((index) => Math.max(index - 1, 0));
					} else if (event.key === "Enter") {
						event.preventDefault();
						activate(filtered[highlighted]);
					}
				}}
			/>
			<div ref={listRef} {...stylex.props(styles.list)}>
				{filtered.length === 0 && (
					<p {...stylex.props(styles.empty)}>{emptyText}</p>
				)}
				{filtered.map((item, index) => {
					const Icon = item.icon;
					const rowStyles = stylex.props(
						styles.row,
						index === highlighted && styles.rowActive,
					);
					const content = (
						<>
							{Icon && (
								<span {...stylex.props(styles.rowIcon)}>
									<Icon size={16} />
								</span>
							)}
							<span {...stylex.props(styles.rowLabel)}>{item.label}</span>
							{item.next && (
								<ChevronRight size={15} {...stylex.props(styles.chevron)} />
							)}
						</>
					);
					return item.href ? (
						<a
							key={item.id}
							href={item.href}
							data-palette-index={index}
							onMouseEnter={() => setHighlighted(index)}
							{...rowStyles}
						>
							{content}
						</a>
					) : (
						<button
							key={item.id}
							type="button"
							data-palette-index={index}
							onMouseEnter={() => setHighlighted(index)}
							onClick={() => activate(item)}
							{...rowStyles}
						>
							{content}
						</button>
					);
				})}
			</div>
		</>
	);
}

/** A single free-text answer, submitted with Enter. */
export function PalettePrompt({
	crumbs = [],
	label,
	placeholder,
	value,
	busy = false,
	inputError,
	error,
	hint,
	onChange,
	onSubmit,
	onBack,
}: {
	crumbs?: Array<string>;
	label: string;
	placeholder?: string;
	value: string;
	busy?: boolean;
	inputError?: string;
	error?: string;
	hint?: ReactNode;
	onChange: (value: string) => void;
	onSubmit: () => void;
	onBack?: () => void;
}) {
	return (
		<>
			<PaletteCrumbs crumbs={crumbs} />
			<PaletteInput
				label={label}
				placeholder={placeholder}
				value={value}
				disabled={busy}
				busy={busy}
				onChange={onChange}
				onBack={onBack}
				onKeyDown={(event) => {
					if (event.key !== "Enter") return;
					event.preventDefault();
					if (!busy && !inputError) onSubmit();
				}}
			/>
			<div {...stylex.props(styles.footer)}>
				{error ? (
					<p {...stylex.props(noticeStyles.error, styles.notice)}>{error}</p>
				) : (
					<p
						{...stylex.props(
							styles.hint,
							Boolean(inputError) && styles.hintError,
						)}
						aria-live="polite"
					>
						{inputError ?? hint}
					</p>
				)}
			</div>
		</>
	);
}

function PaletteInput({
	label,
	placeholder,
	value,
	disabled,
	busy,
	onChange,
	onBack,
	onKeyDown,
}: {
	label?: string;
	placeholder?: string;
	value: string;
	disabled?: boolean;
	busy?: boolean;
	onChange: (value: string) => void;
	onBack?: () => void;
	onKeyDown: (event: KeyboardEvent<HTMLInputElement>) => void;
}) {
	const ref = useRef<HTMLInputElement>(null);
	useEffect(() => {
		ref.current?.focus();
	}, []);
	return (
		<div {...stylex.props(styles.inputRow)}>
			<input
				ref={ref}
				aria-label={label ?? placeholder}
				placeholder={placeholder}
				value={value}
				disabled={disabled}
				autoComplete="off"
				spellCheck={false}
				onChange={(event) => onChange(event.target.value)}
				onKeyDown={(event) => {
					if (event.key === "Backspace" && value === "" && onBack) {
						event.preventDefault();
						onBack();
						return;
					}
					onKeyDown(event);
				}}
				{...stylex.props(styles.input)}
			/>
			{busy && <Loader2 size={14} {...stylex.props(styles.spinner)} />}
		</div>
	);
}

export const paletteStyles = stylex.create({
	card: {
		width: "100%",
		maxWidth: "440px",
		overflow: "hidden",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: colors.surface,
		boxShadow: "0 24px 60px rgba(0,0,0,0.55)",
	},
});

const styles = stylex.create({
	crumbs: {
		display: "flex",
		flexWrap: "wrap",
		gap: "0.375rem",
		paddingInline: space.md,
		paddingTop: space.md,
	},
	crumb: {
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		paddingInline: space.sm,
		paddingBlock: "3px",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.09em",
		color: colors.label,
	},
	inputRow: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.md,
	},
	input: {
		flex: "1",
		minWidth: "0rem",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		paddingBlock: "0.875rem",
		fontFamily: fonts.mono,
		fontSize: "13px",
		color: colors.ink,
		outlineStyle: "none",
		"::placeholder": { color: colors.dim },
	},
	spinner: {
		flexShrink: "0",
		animation: `${spin} 1s linear infinite`,
		color: colors.muted,
	},
	list: {
		display: "flex",
		maxHeight: "320px",
		flexDirection: "column",
		gap: "2px",
		overflowY: "auto",
		padding: "0.375rem",
	},
	row: {
		display: "flex",
		width: "100%",
		cursor: "pointer",
		alignItems: "center",
		gap: space.md,
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		paddingInline: space.md,
		paddingBlock: "0.625rem",
		textAlign: "left",
		textDecorationLine: "none",
		fontFamily: fonts.sans,
		fontSize: "14px",
		color: colors.muted,
	},
	rowActive: { backgroundColor: colors.surfaceRaised, color: colors.ink },
	rowIcon: { display: "inline-flex", flexShrink: "0" },
	rowLabel: {
		flex: "1",
		minWidth: "0rem",
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	chevron: { flexShrink: "0", color: colors.dim },
	empty: {
		margin: "0rem",
		paddingInline: space.md,
		paddingBlock: space.md,
		fontSize: "12px",
		lineHeight: "1.5",
		color: colors.dim,
	},
	footer: { paddingInline: space.md, paddingBlock: space.sm },
	hint: {
		margin: "0rem",
		fontSize: "11px",
		lineHeight: "1.5",
		color: colors.dim,
	},
	hintError: { color: colors.failed },
	notice: { margin: "0rem" },
});
