import * as stylex from "@stylexjs/stylex";
import { useEffect, useRef, useState } from "react";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { RepositoryPicker } from "#/features/dashboard/services/repository-picker";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

export const MANUAL_SELECTOR = /^[\w.-]+\/[\w.-]+$/;
export function SourceRepositoryDialog({
	repositories,
	repoSelector,
	onClose,
	onSelect,
}: {
	repositories: DashboardHomeState["repositories"];
	repoSelector: string;
	onClose: () => void;
	onSelect: (selector: string) => void;
}) {
	const [repoSearch, setRepoSearch] = useState("");
	const [highlightedIndex, setHighlightedIndex] = useState(0);
	const [hoveredIndex, setHoveredIndex] = useState<number | null>(null);
	const repoListRef = useRef<HTMLDivElement>(null);
	const matches = repositories.filter((repository) =>
		repository.fullName.toLowerCase().includes(repoSearch.toLowerCase()),
	);
	const typed = repoSearch.trim();
	const manualEntry =
		MANUAL_SELECTOR.test(typed) &&
		!matches.some((repository) => repository.fullName === typed)
			? { fullName: typed }
			: undefined;
	const filteredRepositories = manualEntry
		? [manualEntry, ...matches]
		: matches;

	useEffect(() => {
		const index = hoveredIndex ?? highlightedIndex;
		const row = repoListRef.current?.querySelector<HTMLElement>(
			`[data-picker-index="${index}"]`,
		);
		row?.scrollIntoView?.({ block: "nearest" });
	}, [highlightedIndex, hoveredIndex]);

	return (
		<Dialog label="Select source repository" onClose={onClose}>
			<div {...stylex.props(dialogStyles.card)}>
				<RepositoryPicker
					actions={[]}
					filteredRepositories={filteredRepositories}
					highlightedIndex={highlightedIndex}
					hoveredIndex={hoveredIndex}
					loading={false}
					onActivateIndex={(index) => {
						const repository = filteredRepositories[index];
						if (repository) onSelect(repository.fullName);
					}}
					onClose={onClose}
					onConfirm={onSelect}
					onRepositorySelect={() => {}}
					onSearchChange={() => {}}
					repoListRef={repoListRef}
					repoSearch={repoSearch}
					repoSelector={repoSelector}
					setHighlightedIndex={setHighlightedIndex}
					setHoveredIndex={setHoveredIndex}
					setRepoSearch={setRepoSearch}
					showEmptyState={filteredRepositories.length === 0}
				/>
			</div>
		</Dialog>
	);
}
