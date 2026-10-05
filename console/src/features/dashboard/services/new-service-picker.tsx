import { Settings } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
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

/** The repository step of the Add menu: picking a repo deploys it. */
export function NewServicePicker({
	state,
	catalogLoading = false,
	initialError,
	onClose,
	onBack,
	onCreated,
	onCreating,
	onCreateFailed,
	confirmRepository = doCreateServiceFast,
}: {
	state: DashboardHomeState;
	catalogLoading?: boolean;
	initialError?: string;
	onClose: () => void;
	onBack?: () => void;
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
	const submittingRef = useRef(false);
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
		if (submittingRef.current) return;
		const selector = (selectorOverride ?? repoSelector).trim();
		if (!selector) return;
		submittingRef.current = true;
		// Commit the picker close and pending node before starting server work.
		flushSync(() => {
			setError(undefined);
			setLoading(true);
			onCreating?.(selector);
		});
		let result: CreateServiceFastResult;
		try {
			result = await confirmRepository({
				data: { repositorySelector: selector },
			});
		} catch (e) {
			const message = formatError(e);
			setError(message);
			setLoading(false);
			submittingRef.current = false;
			onCreateFailed?.(selector, message);
			return;
		}
		onCreated(result);
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
			onBack={onBack}
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
