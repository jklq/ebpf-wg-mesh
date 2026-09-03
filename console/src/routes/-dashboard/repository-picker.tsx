import { Github, Loader2, X } from "lucide-react";
import { useEffect, useRef } from "react";

import { cn } from "#/lib/cn";
import { btnGhost, errorMsg } from "#/lib/ui-classes";

import type { RepositoryPickerProps } from "./types";

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

const skeletonShimmer =
	"absolute inset-0 -translate-x-[120%] animate-skeleton-shimmer bg-gradient-to-r from-transparent via-[rgba(212,205,197,0.08)] to-transparent";

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
			<div className="flex items-center gap-1.5 border-b border-line px-3">
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
						if (event.key === "Enter" && selectableCount > 0) {
							onActivateIndex(highlightedIndex);
						}
					}}
					className="flex-1 border-0 bg-transparent py-2.5 font-[inherit] text-[13px] text-ink outline-none"
				/>
				{loading && (
					<Loader2 size={13} className="shrink-0 animate-spin text-muted" />
				)}
				<button
					type="button"
					className={cn(btnGhost, "px-1.5 py-1")}
					onClick={onClose}
				>
					<X size={14} />
				</button>
			</div>

			<div ref={repoListRef} className="max-h-[260px] overflow-y-auto">
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
						className={cn(
							"m-0 px-3 py-2 text-[11px] text-muted",
							actions.length > 0 && "border-t border-line",
						)}
					>
						No repositories match your search
					</p>
				)}
			</div>

			{error && <p className={cn(errorMsg, "mx-3 my-2")}>{error}</p>}
		</>
	);
}

function RepositoryPickerSkeleton({ actionsCount }: { actionsCount: number }) {
	const rows = actionsCount > 0 ? 7 : 8;
	return (
		<output
			className="flex min-h-[260px] flex-col"
			aria-label="Loading repositories"
			aria-busy="true"
		>
			{skeletonRowIDs.slice(0, rows).map((rowID, index) => (
				<div
					key={rowID}
					className={cn(
						"flex min-h-8 w-full flex-1 items-center gap-2 px-3 py-[7px]",
						index > 0 || actionsCount > 0
							? "border-t border-line"
							: "border-t-0",
					)}
				>
					<span className="relative inline-block size-[13px] shrink-0 overflow-hidden rounded-[2px] bg-[rgba(80,76,71,0.42)]">
						<span className={skeletonShimmer} />
					</span>
					<span
						className={cn(
							"relative inline-block h-3 overflow-hidden rounded-[1px] bg-[rgba(80,76,71,0.42)]",
							index % 2 === 0 ? "w-[68%]" : "w-[52%]",
						)}
					>
						<span className={skeletonShimmer} />
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
						className={pickerRowClass({
							active: highlighted,
							pickerIndex,
						})}
					>
						<span
							className={cn(
								"inline-flex shrink-0",
								highlighted ? "text-ink" : "text-muted",
							)}
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
			className={pickerRowClass({ active: rowActive, pickerIndex })}
		>
			{active ? (
				<Loader2 size={13} className="shrink-0 animate-spin text-ink" />
			) : (
				<Github
					size={13}
					className={cn("shrink-0", rowActive ? "text-ink" : "text-muted")}
				/>
			)}
			{repositoryFullName}
		</button>
	);
}

function pickerRowClass(input: {
	active?: boolean;
	pickerIndex: number;
}): string {
	return cn(
		"flex w-full cursor-pointer items-center gap-2 border-x-0 border-b-0 px-3 py-[7px] text-left font-mono text-xs text-ink no-underline",
		input.pickerIndex > 0 ? "border-t border-line" : "border-t-0",
		input.active ? "bg-surface-raised" : "bg-transparent",
	);
}
