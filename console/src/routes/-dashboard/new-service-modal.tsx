import { Settings } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type {
	CreateServiceFastResult,
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";
import {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "#/lib/dashboard/core/defaults";

import { RepositoryPicker } from "./repository-picker";
import { doCreateServiceFast } from "./server-fns";
import { formatError } from "./service-utils";
import type { ConfirmRepositoryFn, PickerAction } from "./types";

export function NewServiceModal({
	state,
	catalogLoading = false,
	onClose,
	onCreated,
	confirmRepository = doCreateServiceFast,
}: {
	state: DashboardHomeState;
	catalogLoading?: boolean;
	onClose: () => void;
	onCreated: (result: CreateServiceFastResult) => void;
	confirmRepository?: ConfirmRepositoryFn;
}) {
	const [repoSelector, setRepoSelector] = useState(
		state.onboarding.repositorySelector,
	);
	const [loading, setLoading] = useState(false);
	const [cpuMillis, setCpuMillis] = useState(DEFAULT_SERVICE_CPU_MILLIS);
	const [memoryMebibytes, setMemoryMebibytes] = useState(
		DEFAULT_SERVICE_MEMORY_MEBIBYTES,
	);
	const [error, setError] = useState<string>();
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
		if (
			!Number.isSafeInteger(cpuMillis) ||
			cpuMillis < DEFAULT_SERVICE_CPU_MILLIS ||
			!Number.isSafeInteger(memoryMebibytes) ||
			memoryMebibytes < DEFAULT_SERVICE_MEMORY_MEBIBYTES
		) {
			setError(
				`CPU must be at least ${DEFAULT_SERVICE_CPU_MILLIS}m and memory at least ${DEFAULT_SERVICE_MEMORY_MEBIBYTES} MiB.`,
			);
			return;
		}
		setError(undefined);
		setLoading(true);
		try {
			const result = await confirmRepository({
				data: {
					repositorySelector: selector,
					cpuMillis,
					memoryMebibytes,
				},
			});
			onCreated(result);
		} catch (e) {
			setError(formatError(e));
			setLoading(false);
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
		<div
			className="modal-overlay"
			role="dialog"
			aria-modal="true"
			tabIndex={-1}
			onClick={(event) => {
				if (event.target === event.currentTarget) onClose();
			}}
			onKeyDown={(event) => {
				if (event.key === "Escape") onClose();
			}}
		>
			<div className="modal-card">
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
				<div
					style={{
						display: "grid",
						gridTemplateColumns: "1fr 1fr",
						gap: 12,
						padding: 12,
						borderTop: "1px solid var(--border)",
					}}
				>
					<label className="field-label">
						CPU request (millicores)
						<input
							type="number"
							min={DEFAULT_SERVICE_CPU_MILLIS}
							step={1}
							className="field-input"
							value={cpuMillis}
							onChange={(event) =>
								setCpuMillis(event.currentTarget.valueAsNumber)
							}
						/>
					</label>
					<label className="field-label">
						Memory request (MiB)
						<input
							type="number"
							min={DEFAULT_SERVICE_MEMORY_MEBIBYTES}
							step={1}
							className="field-input"
							value={memoryMebibytes}
							onChange={(event) =>
								setMemoryMebibytes(event.currentTarget.valueAsNumber)
							}
						/>
					</label>
				</div>
			</div>
		</div>
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
