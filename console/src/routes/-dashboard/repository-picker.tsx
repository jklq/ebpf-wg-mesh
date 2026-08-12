import { Github, Loader2, X } from "lucide-react";
import { type CSSProperties, useEffect, useRef } from "react";

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
			<div
				style={{
					display: "flex",
					alignItems: "center",
					gap: 6,
					padding: "0 12px",
					borderBottom: "1px solid var(--border)",
				}}
			>
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
					style={{
						flex: 1,
						background: "transparent",
						border: "none",
						outline: "none",
						padding: "10px 0",
						fontSize: 13,
						color: "var(--text)",
						fontFamily: "inherit",
					}}
				/>
				{loading && (
					<Loader2
						size={13}
						style={{
							animation: "spin 1s linear infinite",
							color: "var(--text-muted)",
							flexShrink: 0,
						}}
					/>
				)}
				<button
					type="button"
					className="btn-ghost"
					onClick={onClose}
					style={{ padding: "4px 6px" }}
				>
					<X size={14} />
				</button>
			</div>

			<div ref={repoListRef} style={{ maxHeight: 260, overflowY: "auto" }}>
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
						style={{
							margin: 0,
							padding: "8px 12px",
							fontSize: 11,
							color: "var(--text-muted)",
							borderTop:
								actions.length > 0 ? "1px solid var(--border)" : "none",
						}}
					>
						No repositories match your search
					</p>
				)}
			</div>

			{error && (
				<p className="error-msg" style={{ margin: "8px 12px" }}>
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
			className="repo-picker-skeleton"
			aria-label="Loading repositories"
			aria-busy="true"
		>
			{skeletonRowIDs.slice(0, rows).map((rowID, index) => (
				<div
					key={rowID}
					className="repo-picker-skeleton-row"
					style={{ borderTop: index > 0 || actionsCount > 0 ? undefined : 0 }}
				>
					<span className="repo-picker-skeleton-icon" />
					<span
						className="repo-picker-skeleton-line"
						style={{ width: `${index % 2 === 0 ? 68 : 52}%` }}
					/>
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
						style={pickerRowStyle({
							active: highlighted,
							pickerIndex,
						})}
					>
						<Icon size={13} style={pickerRowIconStyle(highlighted)} />
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
			style={pickerRowStyle({ active: rowActive, pickerIndex })}
		>
			{active ? (
				<Loader2
					size={13}
					style={{
						animation: "spin 1s linear infinite",
						flexShrink: 0,
						color: "var(--text)",
					}}
				/>
			) : (
				<Github size={13} style={pickerRowIconStyle(rowActive)} />
			)}
			{repositoryFullName}
		</button>
	);
}

function pickerRowStyle(input: {
	active?: boolean;
	interactive?: boolean;
	pickerIndex: number;
}): CSSProperties {
	return {
		display: "flex",
		alignItems: "center",
		gap: 8,
		width: "100%",
		padding: "7px 12px",
		background: input.active ? "var(--surface-raised)" : "transparent",
		border: "none",
		borderTop: input.pickerIndex > 0 ? "1px solid var(--border)" : "none",
		color: "var(--text)",
		cursor: input.interactive === false ? undefined : "pointer",
		fontFamily: "var(--font-mono)",
		fontSize: 12,
		textAlign: "left",
		textDecoration: "none",
	};
}

function pickerRowIconStyle(active: boolean): CSSProperties {
	return {
		flexShrink: 0,
		color: active ? "var(--text)" : "var(--text-muted)",
	};
}
