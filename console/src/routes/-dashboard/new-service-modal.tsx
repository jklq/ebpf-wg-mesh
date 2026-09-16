import { Settings } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type {
	CreateServiceFastResult,
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";
import { modalCard } from "#/lib/ui-classes";

import { RepositoryPicker } from "./repository-picker";
import { doCreateServiceFast } from "./server-fns";
import { formatError } from "./service-utils";
import type { ConfirmRepositoryFn, PickerAction } from "./types";
import { ModalOverlay } from "./ui";

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
	const [builder, setBuilder] = useState<
		"BUILDER_KIND_RAILPACK" | "BUILDER_KIND_DOCKERFILE"
	>("BUILDER_KIND_RAILPACK");
	const [dockerfilePath, setDockerfilePath] = useState("");
	const [contextDir, setContextDir] = useState("");
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
				data: {
					repositorySelector: selector,
					builder,
					...(builder === "BUILDER_KIND_DOCKERFILE" &&
					dockerfilePath.trim() !== ""
						? { dockerfilePath: dockerfilePath.trim() }
						: {}),
					...(contextDir.trim() !== ""
						? { contextDir: contextDir.trim() }
						: {}),
				},
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
		<ModalOverlay onClose={onClose}>
			<div className={modalCard}>
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
				<div className="flex flex-col gap-2 border-t border-line px-3 py-2.5">
					<div
						className="flex items-center gap-4 text-xs text-muted"
						role="radiogroup"
						aria-label="Builder"
					>
						<span className="font-condensed text-[11px] font-bold uppercase tracking-[0.09em]">
							Builder
						</span>
						<label className="inline-flex cursor-pointer items-center gap-1.5">
							<input
								type="radio"
								name="new-service-builder"
								checked={builder === "BUILDER_KIND_RAILPACK"}
								onChange={() => setBuilder("BUILDER_KIND_RAILPACK")}
							/>
							Railpack
						</label>
						<label className="inline-flex cursor-pointer items-center gap-1.5">
							<input
								type="radio"
								name="new-service-builder"
								checked={builder === "BUILDER_KIND_DOCKERFILE"}
								onChange={() => setBuilder("BUILDER_KIND_DOCKERFILE")}
							/>
							Dockerfile
						</label>
					</div>
					{builder === "BUILDER_KIND_DOCKERFILE" ? (
						<div className="grid grid-cols-2 gap-2 max-sm:grid-cols-1">
							<input
								autoComplete="off"
								spellCheck={false}
								placeholder="Dockerfile path"
								aria-label="Dockerfile path"
								value={dockerfilePath}
								onChange={(event) => setDockerfilePath(event.target.value)}
								className="border border-line bg-transparent px-2 py-1.5 font-mono text-xs text-ink outline-none placeholder:text-dim"
							/>
							<input
								autoComplete="off"
								spellCheck={false}
								placeholder="Context dir (.)"
								aria-label="Build context directory"
								value={contextDir}
								onChange={(event) => setContextDir(event.target.value)}
								className="border border-line bg-transparent px-2 py-1.5 font-mono text-xs text-ink outline-none placeholder:text-dim"
							/>
						</div>
					) : (
						<input
							autoComplete="off"
							spellCheck={false}
							placeholder="Application directory (.)"
							aria-label="Application directory"
							value={contextDir}
							onChange={(event) => setContextDir(event.target.value)}
							className="border border-line bg-transparent px-2 py-1.5 font-mono text-xs text-ink outline-none placeholder:text-dim"
						/>
					)}
				</div>
			</div>
		</ModalOverlay>
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
