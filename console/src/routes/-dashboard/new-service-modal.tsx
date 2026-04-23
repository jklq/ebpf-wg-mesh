import { Loader2, Settings, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type {
	DashboardHomeState,
} from "#/lib/dashboard/core/types.server";

import { RepositoryPicker } from "./repository-picker";
import { doConfirmRepository } from "./server-fns";
import { formatError } from "./service-utils";
import type {
	ConfirmRepositoryFn,
	NewServiceStep,
	PickerAction,
} from "./types";

export function NewServiceModal({
	state,
	onClose,
	onCreated,
	confirmRepository = doConfirmRepository,
}: {
	state: DashboardHomeState;
	onClose: () => void;
	onCreated: (state?: DashboardHomeState) => void;
	confirmRepository?: ConfirmRepositoryFn;
}) {
	const [step, setStep] = useState<NewServiceStep>("repo");
	const [repoSelector, setRepoSelector] = useState(
		state.onboarding.repositorySelector,
	);
	const [loading, setLoading] = useState(false);
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
		const selector = (selectorOverride ?? repoSelector).trim();
		if (!selector) return;
		setError(undefined);
		setLoading(true);
		setStep("deploying");
		try {
			const nextState = await confirmRepository({
				data: {
					repositorySelector: selector,
				},
			});
			onCreated(isDashboardHomeState(nextState) ? nextState : undefined);
		} catch (e) {
			setError(formatError(e));
			setStep("repo");
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
				{step !== "repo" && (
					<NewServiceHeader onClose={onClose} step={step} />
				)}

				<div style={step !== "repo" ? { padding: "20px" } : {}}>
					{step === "repo" && (
						<RepositoryPicker
							actions={pickerActions}
							error={error}
							filteredRepositories={filteredRepositories}
							highlightedIndex={highlightedIndex}
							hoveredIndex={hoveredIndex}
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
								filteredRepositories.length === 0 &&
								state.repositories.length > 0
							}
						/>
					)}

					{step === "deploying" && <DeployingStep />}
				</div>
			</div>
		</div>
	);
}

function isDashboardHomeState(value: unknown): value is DashboardHomeState {
	return (
		typeof value === "object" &&
		value !== null &&
		Array.isArray((value as Partial<DashboardHomeState>).services)
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

function NewServiceHeader({
	onClose,
	step,
}: {
	onClose: () => void;
	step: NewServiceStep;
}) {
	return (
		<div
			style={{
				display: "flex",
				alignItems: "center",
				justifyContent: "space-between",
				padding: "16px 20px 12px",
				borderBottom: "1px solid var(--border)",
			}}
		>
			<div>
				<h2
					style={{
						margin: 0,
						fontSize: 16,
						fontWeight: 700,
						letterSpacing: "0.06em",
						textTransform: "uppercase",
						fontFamily: "'Barlow Condensed', sans-serif",
						color: "var(--text)",
					}}
				>
					{step === "deploying" ? "Deploying…" : "New service"}
				</h2>
				<p
					style={{
						margin: "2px 0 0",
						fontSize: 12,
						color: "var(--text-muted)",
					}}
				>
					{step === "deploying" && "Build queued, the canvas will update."}
				</p>
			</div>
			<button
				type="button"
				className="btn-ghost"
				onClick={onClose}
				style={{ padding: "4px 6px" }}
			>
				<X size={16} />
			</button>
		</div>
	);
}

function DeployingStep() {
	return (
		<div
			style={{
				display: "flex",
				flexDirection: "column",
				alignItems: "center",
				gap: 16,
				padding: "24px 0",
			}}
		>
			<Loader2
				size={32}
				color="var(--accent)"
				style={{ animation: "spin 1s linear infinite" }}
			/>
			<p style={{ margin: 0, fontSize: 13, color: "var(--text-muted)" }}>
				Creating service and queuing build…
			</p>
		</div>
	);
}
