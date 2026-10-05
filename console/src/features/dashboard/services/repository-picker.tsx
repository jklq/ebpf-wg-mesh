import { Button } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";

const shimmerAnimation = stylex.keyframes({
	from: { transform: "translateX(-120%)" },
	to: { transform: "translateX(120%)" },
});

import * as stylex from "@stylexjs/stylex";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { Github, Loader2, X } from "lucide-react";
import { useEffect, useRef } from "react";

import type { RepositoryPickerProps } from "#/features/dashboard/shared/types";

const skeletonRowIDs = [
	"repository-skeleton-1",
	"repository-skeleton-2",
	"repository-skeleton-3",
	"repository-skeleton-4",
	"repository-skeleton-5",
	"repository-skeleton-6",
	"repository-skeleton-7",
	"repository-skeleton-8",
] as const;

const skeletonShimmer = stylex.create({
	root: {
		position: "absolute",
		inset: "0rem",
		animation: `${shimmerAnimation} 1.25s ease-in-out infinite`,
		backgroundImage:
			"linear-gradient(to right in oklab, transparent 0%, rgba(212,205,197,0.08) 50%, transparent 100%)",
	},
});

export function RepositoryPicker({
	actions,
	error,
	filteredRepositories,
	highlightedIndex,
	hoveredIndex,
	catalogLoading = false,
	loading,
	onActivateIndex,
	onClose,
	onBack,
	onConfirm,
	onRepositorySelect,
	onSearchChange,
	repoListRef,
	repoSearch,
	repoSelector,
	setHighlightedIndex,
	setHoveredIndex,
	setRepoSearch,
	showEmptyState,
}: RepositoryPickerProps) {
	const selectableCount = actions.length + filteredRepositories.length;
	const activeIndex = hoveredIndex ?? highlightedIndex;
	const searchRef = useRef<HTMLInputElement>(null);

	useEffect(() => {
		searchRef.current?.focus();
	}, []);

	return (
		<>
			<div {...stylex.props(styles.searchBar)}>
				<input
					ref={searchRef}
					autoComplete="off"
					spellCheck={false}
					placeholder="Search repositories…"
					value={repoSearch}
					onChange={(event) => {
						setRepoSearch(event.target.value);
						setHighlightedIndex(0);
						onSearchChange();
					}}
					onKeyDown={(event) => {
						if (event.key === "Escape") {
							event.preventDefault();
							event.stopPropagation();
							onClose();
							return;
						}
						if (event.key === "Backspace" && repoSearch === "" && onBack) {
							event.preventDefault();
							onBack();
							return;
						}
						if (event.key === "ArrowDown") {
							event.preventDefault();
							if (selectableCount > 0) {
								setHighlightedIndex((index) =>
									Math.min(index + 1, selectableCount - 1),
								);
							}
							return;
						}
						if (event.key === "ArrowUp") {
							event.preventDefault();
							if (selectableCount > 0) {
								setHighlightedIndex((index) => Math.max(index - 1, 0));
							}
							return;
						}
						if (event.key === "Enter") {
							// Without this, the Enter keypress lands on whatever regains
							// focus once the picker closes and reopens it.
							event.preventDefault();
							if (selectableCount > 0) onActivateIndex(highlightedIndex);
						}
					}}
					{...stylex.props(styles.searchInput)}
				/>
				{loading && <Loader2 size={13} {...stylex.props(styles.spinner)} />}
				<Button
					type="button"
					variant="ghost"
					styles={[styles.closeButton]}
					onClick={onClose}
				>
					<X size={14} />
				</Button>
			</div>

			<div ref={repoListRef} {...stylex.props(styles.repositoryList)}>
				{(() => {
					const searching = repoSearch.length > 0;
					const actionsOffset = searching ? filteredRepositories.length : 0;
					const reposOffset = searching ? 0 : actions.length;

					const repoRows = filteredRepositories.map((repository, index) => {
						const pickerIndex = reposOffset + index;
						const active = repoSelector === repository.fullName && loading;
						return (
							<PickerRepositoryRow
								key={repository.fullName}
								active={active}
								highlighted={pickerIndex === activeIndex}
								loading={loading}
								onHover={setHoveredIndex}
								onConfirm={onConfirm}
								onRepositorySelect={onRepositorySelect}
								pickerIndex={pickerIndex}
								repositoryFullName={repository.fullName}
							/>
						);
					});

					const actionRows = (
						<PickerActions
							actions={actions}
							highlightedIndex={activeIndex}
							indexOffset={actionsOffset}
							onHoveredIndex={setHoveredIndex}
						/>
					);

					return searching ? (
						<>
							{repoRows}
							{actionRows}
						</>
					) : (
						<>
							{actionRows}
							{repoRows}
						</>
					);
				})()}

				{catalogLoading && filteredRepositories.length === 0 && (
					<RepositoryPickerSkeleton actionsCount={actions.length} />
				)}

				{showEmptyState && !catalogLoading && (
					<p
						{...stylex.props([
							styles.emptySearch,
							actions.length > 0 && styles.borderedRow,
						])}
					>
						No repositories match your search
					</p>
				)}
			</div>

			{error && (
				<p {...stylex.props([noticeStyles.error, styles.errorMessage])}>
					{error}
				</p>
			)}
		</>
	);
}

function RepositoryPickerSkeleton({ actionsCount }: { actionsCount: number }) {
	const rows = actionsCount > 0 ? 7 : 8;
	return (
		<output
			{...stylex.props(styles.loadingList)}
			aria-label="Loading repositories"
			aria-busy="true"
		>
			{skeletonRowIDs.slice(0, rows).map((rowID, index) => (
				<div
					key={rowID}
					{...stylex.props([
						styles.skeletonRow,
						index > 0 || actionsCount > 0
							? styles.borderedRow
							: styles.firstRow,
					])}
				>
					<span {...stylex.props(styles.skeletonIcon)}>
						<span {...stylex.props(skeletonShimmer.root)} />
					</span>
					<span
						{...stylex.props([
							styles.skeletonName,
							index % 2 === 0
								? styles.longSkeletonName
								: styles.shortSkeletonName,
						])}
					>
						<span {...stylex.props(skeletonShimmer.root)} />
					</span>
				</div>
			))}
		</output>
	);
}

function PickerActions({
	actions,
	highlightedIndex,
	indexOffset,
	onHoveredIndex,
}: {
	actions: RepositoryPickerProps["actions"];
	highlightedIndex: number;
	indexOffset: number;
	onHoveredIndex: RepositoryPickerProps["setHoveredIndex"];
}) {
	return (
		<>
			{actions.map((action, i) => {
				const pickerIndex = indexOffset + i;
				const highlighted = pickerIndex === highlightedIndex;
				const Icon = action.icon;
				return (
					<a
						key={action.label}
						data-picker-index={pickerIndex}
						href={action.href}
						target={action.external ? "_blank" : undefined}
						rel={action.external ? "noreferrer" : undefined}
						onMouseEnter={() => onHoveredIndex(pickerIndex)}
						onMouseLeave={() => onHoveredIndex(null)}
						{...stylex.props(
							pickerRowStyles({
								active: highlighted,
								pickerIndex,
							}),
						)}
					>
						<span
							{...stylex.props([
								styles.actionIcon,
								highlighted
									? styles.highlightedActionIcon
									: styles.idleActionIcon,
							])}
						>
							<Icon size={13} />
						</span>
						{action.label}
					</a>
				);
			})}
		</>
	);
}

function PickerRepositoryRow({
	active,
	highlighted,
	loading,
	onHover,
	onConfirm,
	onRepositorySelect,
	pickerIndex,
	repositoryFullName,
}: {
	active: boolean;
	highlighted: boolean;
	loading: boolean;
	onHover: RepositoryPickerProps["setHoveredIndex"];
	onConfirm: (selector: string) => void;
	onRepositorySelect: (selector: string) => void;
	pickerIndex: number;
	repositoryFullName: string;
}) {
	const rowActive = active || highlighted;

	return (
		<button
			data-picker-index={pickerIndex}
			type="button"
			disabled={loading}
			onClick={() => {
				onRepositorySelect(repositoryFullName);
				onConfirm(repositoryFullName);
			}}
			onMouseEnter={() => onHover(pickerIndex)}
			onMouseLeave={() => onHover(null)}
			{...stylex.props(pickerRowStyles({ active: rowActive, pickerIndex }))}
		>
			{active ? (
				<Loader2 size={13} {...stylex.props(styles.creatingIcon)} />
			) : (
				<Github
					size={13}
					{...stylex.props([
						styles.repositoryIcon,
						rowActive ? styles.highlightedActionIcon : styles.idleActionIcon,
					])}
				/>
			)}
			{repositoryFullName}
		</button>
	);
}

function pickerRowStyles(input: {
	active?: boolean;
	pickerIndex: number;
}): stylex.StyleXStyles {
	return [
		styles.repositoryRow,
		input.pickerIndex > 0 ? styles.borderedRow : styles.firstRow,
		input.active ? styles.selectedRow : styles.idleRow,
	];
}

const styles = stylex.create({
	searchBar: {
		display: "flex",
		alignItems: "center",
		gap: "0.375rem",
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.md,
	},
	searchInput: {
		flex: "1",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		paddingBlock: "0.625rem",
		fontFamily: "inherit",
		fontSize: "13px",
		color: colors.ink,
		outlineStyle: "none",
	},
	spinner: {
		flexShrink: "0",
		animation: `${spin} 1s linear infinite`,
		color: colors.muted,
	},
	closeButton: { paddingInline: "0.375rem", paddingBlock: space.xs },
	repositoryList: { maxHeight: "260px", overflowY: "auto" },
	emptySearch: {
		margin: "0rem",
		paddingInline: space.md,
		paddingBlock: space.sm,
		fontSize: "11px",
		color: colors.muted,
	},
	borderedRow: {
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: colors.line,
	},
	errorMessage: { marginInline: space.md, marginBlock: space.sm },
	loadingList: { display: "flex", minHeight: "260px", flexDirection: "column" },
	skeletonRow: {
		display: "flex",
		minHeight: "2rem",
		width: "100%",
		flex: "1",
		alignItems: "center",
		gap: space.sm,
		paddingInline: space.md,
		paddingBlock: "7px",
	},
	firstRow: { borderTopStyle: "solid", borderTopWidth: "0px" },
	skeletonIcon: {
		position: "relative",
		display: "inline-block",
		width: "13px",
		height: "13px",
		flexShrink: "0",
		overflow: "hidden",
		borderRadius: "2px",
		backgroundColor: "rgba(80,76,71,0.42)",
	},
	skeletonName: {
		position: "relative",
		display: "inline-block",
		height: "0.75rem",
		overflow: "hidden",
		borderRadius: shape.control,
		backgroundColor: "rgba(80,76,71,0.42)",
	},
	longSkeletonName: { width: "68%" },
	shortSkeletonName: { width: "52%" },
	actionIcon: { display: "inline-flex", flexShrink: "0" },
	highlightedActionIcon: { color: colors.ink },
	idleActionIcon: { color: colors.muted },
	creatingIcon: {
		flexShrink: "0",
		animation: `${spin} 1s linear infinite`,
		color: colors.ink,
	},
	repositoryIcon: { flexShrink: "0" },
	repositoryRow: {
		display: "flex",
		width: "100%",
		cursor: "pointer",
		alignItems: "center",
		gap: space.sm,
		borderInlineStyle: "solid",
		borderInlineWidth: "0px",
		borderBottomStyle: "solid",
		borderBottomWidth: "0px",
		paddingInline: space.md,
		paddingBlock: "7px",
		textAlign: "left",
		fontFamily: fonts.mono,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.ink,
		textDecorationLine: "none",
	},
	selectedRow: { backgroundColor: colors.surfaceRaised },
	idleRow: { backgroundColor: "transparent" },
});
