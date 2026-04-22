import { CheckCircle2, Github, Loader2, Settings, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type {
	DashboardHomeState,
	DashboardOnboardingDraft,
} from "#/lib/dashboard/core/types.server";

import { RepositoryPicker } from "./repository-picker";
import { doConfirmRepository, doInspectRepository } from "./server-fns";
import { formatError, repositoryInspectionBlocker } from "./service-utils";
import type {
	ConfirmRepositoryFn,
	InspectRepositoryFn,
	NewServiceStep,
	PickerAction,
} from "./types";

export function NewServiceModal({
	state,
	onClose,
	onCreated,
	inspectRepository = doInspectRepository,
	confirmRepository = doConfirmRepository,
}: {
	state: DashboardHomeState;
	onClose: () => void;
	onCreated: () => void;
	inspectRepository?: InspectRepositoryFn;
	confirmRepository?: ConfirmRepositoryFn;
}) {
	const [step, setStep] = useState<NewServiceStep>("repo");
	const [repoSelector, setRepoSelector] = useState(
		state.onboarding.repositorySelector,
	);
	const [draft, setDraft] = useState<DashboardOnboardingDraft | null>(null);
	const [trackedRef, setTrackedRef] = useState(state.onboarding.trackedRef);
	const [dockerfilePath, setDockerfilePath] = useState(
		state.onboarding.dockerfilePath,
	);
	const [contextDir, setContextDir] = useState(state.onboarding.contextDir);
	const [containerPort, setContainerPort] = useState(
		state.onboarding.containerPort || "8080",
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
		setDraft(null);
	};

	const handleRepositorySelect = (selector: string) => {
		if (selector !== repoSelector) {
			clearRepositoryCheckFeedback();
		}
		setRepoSelector(selector);
	};

	const handleInspect = async (selectorOverride?: string) => {
		const selector = (selectorOverride ?? repoSelector).trim();
		if (!selector) return;
		setError(undefined);
		setLoading(true);
		try {
			const result = await inspectRepository({
				data: { repositorySelector: selector },
			});
			const nextDraft = result.onboarding;
			setDraft(nextDraft);
			setTrackedRef(nextDraft.trackedRef);
			setDockerfilePath(nextDraft.dockerfilePath);
			setContextDir(nextDraft.contextDir);
			setContainerPort(nextDraft.containerPort || "8080");
			const blocker = repositoryInspectionBlocker(result);
			if (blocker) {
				setError(blocker);
				return;
			}
			setStep("configure");
		} catch (e) {
			setError(formatError(e));
		} finally {
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
			handleInspect(repo.fullName);
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

	const handleConfirm = async () => {
		setError(undefined);
		setLoading(true);
		setStep("deploying");
		try {
			await confirmRepository({
				data: {
					repositorySelector: repoSelector.trim(),
					trackedRef,
					dockerfilePath,
					contextDir,
					containerPort,
				},
			});
			onCreated();
		} catch (e) {
			setError(formatError(e));
			setStep("configure");
			setLoading(false);
		}
	};

	return (
		<div
			className="modal-overlay"
			onClick={(event) => {
				if (event.target === event.currentTarget) onClose();
			}}
		>
			<div className="modal-card">
				{step !== "repo" && (
					<NewServiceHeader
						onClose={onClose}
						repoSelector={repoSelector}
						step={step}
					/>
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
							onInspect={handleInspect}
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

					{step === "configure" && (
						<ConfigureServiceStep
							containerPort={containerPort}
							dockerfilePath={dockerfilePath}
							draft={draft}
							error={error}
							loading={loading}
							onCancel={onClose}
							onConfirm={handleConfirm}
							onContainerPortChange={setContainerPort}
							onContextDirChange={setContextDir}
							onDockerfilePathChange={setDockerfilePath}
							onTrackedRefChange={setTrackedRef}
							onBack={() => setStep("repo")}
							contextDir={contextDir}
							trackedRef={trackedRef}
						/>
					)}

					{step === "deploying" && <DeployingStep />}
				</div>
			</div>
		</div>
	);
}

function buildPickerActions(state: DashboardHomeState): PickerAction[] {
	const actions: PickerAction[] = [];
	if (!state.githubAccount && state.githubLoginURL) {
		actions.push({
			href: state.githubLoginURL,
			label: "Connect GitHub to load repositories",
			icon: Github,
		});
	}
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
	repoSelector,
	step,
}: {
	onClose: () => void;
	repoSelector: string;
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
						fontSize: 15,
						fontWeight: 700,
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
					{step === "configure" &&
						`Configure build settings for ${repoSelector}`}
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

function ConfigureServiceStep({
	containerPort,
	contextDir,
	dockerfilePath,
	draft,
	error,
	loading,
	onBack,
	onCancel,
	onConfirm,
	onContainerPortChange,
	onContextDirChange,
	onDockerfilePathChange,
	onTrackedRefChange,
	trackedRef,
}: {
	containerPort: string;
	contextDir: string;
	dockerfilePath: string;
	draft: DashboardOnboardingDraft | null;
	error?: string;
	loading: boolean;
	onBack: () => void;
	onCancel: () => void;
	onConfirm: () => void;
	onContainerPortChange: (value: string) => void;
	onContextDirChange: (value: string) => void;
	onDockerfilePathChange: (value: string) => void;
	onTrackedRefChange: (value: string) => void;
	trackedRef: string;
}) {
	return (
		<div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
			{draft?.repositorySelector && draft.repositorySelector !== "" && (
				<div
					style={{
						padding: "8px 12px",
						background: "var(--healthy-dim)",
						border: "1px solid var(--healthy)",
						borderRadius: 6,
						fontSize: 12,
						color: "var(--healthy)",
						display: "flex",
						alignItems: "center",
						gap: 6,
					}}
				>
					<CheckCircle2 size={12} />
					Repository access confirmed
				</div>
			)}

			<div>
				<label className="field-label">Branch</label>
				<input
					className="field-input"
					value={trackedRef}
					onChange={(event) => onTrackedRefChange(event.target.value)}
					placeholder="main"
				/>
			</div>

			<div>
				<label className="field-label">Dockerfile path</label>
				<input
					className="field-input"
					value={dockerfilePath}
					onChange={(event) => onDockerfilePathChange(event.target.value)}
					placeholder="Dockerfile"
				/>
			</div>

			<div>
				<label className="field-label">Build context directory</label>
				<input
					className="field-input"
					value={contextDir}
					onChange={(event) => onContextDirChange(event.target.value)}
					placeholder="."
				/>
			</div>

			<div>
				<label className="field-label">Container port</label>
				<input
					className="field-input"
					value={containerPort}
					onChange={(event) => onContainerPortChange(event.target.value)}
					placeholder="8080"
					inputMode="numeric"
				/>
			</div>

			{error && <p className="error-msg">{error}</p>}

			<div
				style={{
					display: "flex",
					justifyContent: "space-between",
					gap: 8,
					paddingTop: 4,
				}}
			>
				<button type="button" className="btn-ghost" onClick={onBack}>
					← Back
				</button>
				<div style={{ display: "flex", gap: 8 }}>
					<button type="button" className="btn-secondary" onClick={onCancel}>
						Cancel
					</button>
					<button
						type="button"
						className="btn-primary"
						onClick={onConfirm}
						disabled={loading}
					>
						{loading && (
							<Loader2
								size={13}
								style={{ animation: "spin 1s linear infinite" }}
							/>
						)}
						Deploy service
					</button>
				</div>
			</div>
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
