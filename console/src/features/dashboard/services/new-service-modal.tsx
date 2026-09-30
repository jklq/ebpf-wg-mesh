import * as stylex from "@stylexjs/stylex";
import { Settings } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { RepositoryPicker } from "#/features/dashboard/services/repository-picker";
import type {
	ConfirmRepositoryFn,
	PickerAction,
} from "#/features/dashboard/shared/types";
import type {
	CreateServiceFastResult,
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";
import { doCreateServiceFast } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

export function NewServiceModal({
	state,
	catalogLoading = false,
	initialError,
	onClose,
	onCreated,
	onCreating,
	onCreateFailed,
	confirmRepository = doCreateServiceFast,
}: {
	state: DashboardHomeState;
	catalogLoading?: boolean;
	initialError?: string;
	onClose: () => void;
	onCreated: (result: CreateServiceFastResult) => void;
	onCreating?: (selector: string) => void;
	onCreateFailed?: (selector: string, error: string) => void;
	confirmRepository?: ConfirmRepositoryFn;
}) {
	const [repoSelector, setRepoSelector] = useState(
		state.onboarding.repositorySelector,
	);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<string | undefined>(initialError);
	const [repoSearch, setRepoSearch] = useState("");
	const [highlightedIndex, setHighlightedIndex] = useState(0);
	const [hoveredIndex, setHoveredIndex] = useState<number | null>(null);
	const repoListRef = useRef<HTMLDivElement>(null);
	const filteredRepositories = state.repositories.filter((repo) =>
		repo.fullName.toLowerCase().includes(repoSearch.toLowerCase()),
	);
	const pickerActions = buildPickerActions(state);
	const pickerSelectableCount =
		pickerActions.length + filteredRepositories.length;

	useEffect(() => {
		const index = hoveredIndex ?? highlightedIndex;
		const el = repoListRef.current?.querySelector<HTMLElement>(
			`[data-picker-index="${index}"]`,
		);
		if (typeof el?.scrollIntoView === "function") {
			el.scrollIntoView({ block: "nearest" });
		}
	}, [highlightedIndex, hoveredIndex]);

	useEffect(() => {
		setHighlightedIndex((index) =>
			pickerSelectableCount === 0
				? 0
				: Math.min(index, pickerSelectableCount - 1),
		);
	}, [pickerSelectableCount]);

	const clearRepositoryCheckFeedback = () => {
		setError(undefined);
	};

	const handleRepositorySelect = (selector: string) => {
		if (selector !== repoSelector) {
			clearRepositoryCheckFeedback();
		}
		setRepoSelector(selector);
	};

	const handleConfirm = async (selectorOverride?: string) => {
		if (loading) return;
		const selector = (selectorOverride ?? repoSelector).trim();
		if (!selector) return;
		setError(undefined);
		onCreating?.(selector);
		setLoading(true);
		try {
			const result = await confirmRepository({
				data: { repositorySelector: selector },
			});
			onCreated(result);
		} catch (e) {
			const message = formatError(e);
			setError(message);
			setLoading(false);
			onCreateFailed?.(selector, message);
		}
	};

	const activatePickerSelection = (index: number) => {
		const searching = repoSearch.length > 0;
		const actionsOffset = searching ? filteredRepositories.length : 0;
		const reposOffset = searching ? 0 : pickerActions.length;

		const repo = filteredRepositories[index - reposOffset];
		if (
			repo &&
			index >= reposOffset &&
			index < reposOffset + filteredRepositories.length
		) {
			handleRepositorySelect(repo.fullName);
			handleConfirm(repo.fullName);
			return;
		}

		const action = pickerActions[index - actionsOffset];
		if (action) {
			if (action.external) {
				window.open(action.href, "_blank", "noopener,noreferrer");
			} else {
				window.location.href = action.href;
			}
		}
	};

	return (
		<Dialog label="New service" onClose={onClose}>
			<div {...stylex.props(dialogStyles.card)}>
				<RepositoryPicker
					actions={pickerActions}
					error={error}
					filteredRepositories={filteredRepositories}
					highlightedIndex={highlightedIndex}
					hoveredIndex={hoveredIndex}
					catalogLoading={catalogLoading}
					loading={loading}
					onActivateIndex={activatePickerSelection}
					onClose={onClose}
					onConfirm={handleConfirm}
					onRepositorySelect={handleRepositorySelect}
					onSearchChange={clearRepositoryCheckFeedback}
					repoListRef={repoListRef}
					repoSearch={repoSearch}
					repoSelector={repoSelector}
					setHighlightedIndex={setHighlightedIndex}
					setHoveredIndex={setHoveredIndex}
					setRepoSearch={setRepoSearch}
					showEmptyState={
						filteredRepositories.length === 0 && state.repositories.length > 0
					}
				/>
			</div>
		</Dialog>
	);
}

function buildPickerActions(state: DashboardHomeState): PickerAction[] {
	const actions: PickerAction[] = [];
	if (state.githubAccount && state.githubInstallURL) {
		actions.push({
			href: state.githubInstallURL,
			label: "Configure GitHub App",
			icon: Settings,
			external: true,
		});
	}
	return actions;
}
