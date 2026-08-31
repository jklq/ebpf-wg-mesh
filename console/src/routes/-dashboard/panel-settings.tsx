import { Github, Pencil, Trash2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { ConfirmDeleteDialog } from "./confirm-delete-dialog";
import { ReplicaScaleControls } from "./replica-scale";
import { RepositoryPicker } from "./repository-picker";
import { doDeleteService, doUpdateService } from "./server-fns";
import { formatError } from "./service-utils";
import { ModalOverlay, PanelSection } from "./ui";
import { useAutoQueuedPersist } from "./use-auto-queued-persist";

const MANUAL_SELECTOR = /^[\w.-]+\/[\w.-]+$/;

type SettingsDraft = {
	repoSelector: string;
	trackedRef: string;
	dockerfilePath: string;
	contextDir: string;
	restartPolicy: "on-failure" | "always" | "never";
	maxRestarts: string;
	windowSeconds: string;
	placementRegion: string;
	maxUnavailable: string;
	maxSurge: string;
	startupTimeoutSeconds: string;
	drainTimeoutSeconds: string;
};

export function PanelSettings({
	service,
	state,
	onSaved,
	onDeleted,
	onSavingChange,
}: {
	service: DashboardServiceRecord;
	state: DashboardHomeState;
	onSaved: (service: DashboardServiceRecord) => void;
	onDeleted: (serviceId: string) => void;
	onSavingChange?: (key: string, saving: boolean) => void;
}) {
	const changedFields = new Set(
		(service.unappliedChanges ?? []).map((change) => change.id),
	);
	const trackedRefId = `service-tracked-ref-${service.id}`;
	const dockerfilePathId = `service-dockerfile-path-${service.id}`;
	const contextDirId = `service-context-dir-${service.id}`;
	const placementRegionId = `service-placement-region-${service.id}`;
	const incoming = settingsDraftFromService(service);
	const [confirmingDelete, setConfirmingDelete] = useState(false);
	const [deleting, setDeleting] = useState(false);
	const [deleteError, setDeleteError] = useState<string>();
	const [pickingRepo, setPickingRepo] = useState(false);
	const reportReplicaSaving = useCallback(
		(saving: boolean) => onSavingChange?.("replicas", saving),
		[onSavingChange],
	);

	const { draft, setDraft, error, saving } = useAutoQueuedPersist({
		serviceId: service.id,
		incoming,
		incomingEpoch: service.specRevision ?? 0,
		enabled: (next) => next.repoSelector.trim() !== "",
		persist: async (next) => {
			const updated = await doUpdateService({
				data: {
					serviceId: service.id,
					repositorySelector: next.repoSelector,
					trackedRef: next.trackedRef,
					dockerfilePath: next.dockerfilePath,
					contextDir: next.contextDir,
					restart: {
						policy: next.restartPolicy,
						maxRestarts: Number(next.maxRestarts) || 0,
						windowSeconds: Number(next.windowSeconds) || 0,
					},
					placementRegion: next.placementRegion,
					rollingStrategy: {
						maxUnavailable: Number(next.maxUnavailable) || 0,
						maxSurge: Number(next.maxSurge) || 0,
						startupTimeoutSeconds:
							Number(next.startupTimeoutSeconds) || 0,
						drainTimeoutSeconds: Number(next.drainTimeoutSeconds) || 0,
					},
				},
			});
			onSaved(updated);
		},
	});

	useEffect(() => {
		onSavingChange?.("settings", saving);
		return () => onSavingChange?.("settings", false);
	}, [onSavingChange, saving]);

	const handleDelete = async () => {
		setDeleting(true);
		setDeleteError(undefined);
		try {
			await doDeleteService({ data: { serviceId: service.id } });
			setConfirmingDelete(false);
			onDeleted(service.id);
		} catch (e) {
			setDeleteError(formatError(e));
			setDeleting(false);
		}
	};

	return (
		<div className="settings-stack">
			<PanelSection
				title="Source"
				lede="The repository and branch this service builds from."
			>
				<div>
					<p className="field-label">Source repo</p>
					<div
						className={`source-repo-card ${changedFields.has("source.repositorySelector") ? "unapplied-field" : ""}`}
					>
						<Github size={16} aria-hidden="true" />
						<span className="source-repo-name">
							{draft.repoSelector || "No repository selected"}
						</span>
						<button
							type="button"
							className="source-repo-edit"
							aria-label="Change source repository"
							title="Change source repository"
							onClick={() => setPickingRepo(true)}
						>
							<Pencil size={14} />
						</button>
					</div>
				</div>

				<div>
					<label className="field-label" htmlFor={trackedRefId}>
						Branch
					</label>
					<input
						id={trackedRefId}
						className={`field-input ${changedFields.has("source.trackedRef") ? "unapplied-field" : ""}`}
						value={draft.trackedRef}
						onChange={(e) =>
							setDraft((current) => ({
								...current,
								trackedRef: e.target.value,
							}))
						}
						placeholder="main"
					/>
				</div>
			</PanelSection>

			<PanelSection
				title="Build"
				lede="How the image is assembled before it reaches the mesh."
			>
				<div>
					<label className="field-label" htmlFor={dockerfilePathId}>
						Dockerfile path
					</label>
					<input
						id={dockerfilePathId}
						className={`field-input ${changedFields.has("source.buildRecipe.dockerfilePath") ? "unapplied-field" : ""}`}
						value={draft.dockerfilePath}
						onChange={(e) =>
							setDraft((current) => ({
								...current,
								dockerfilePath: e.target.value,
							}))
						}
						placeholder="Dockerfile"
					/>
				</div>

				<div>
					<label className="field-label" htmlFor={contextDirId}>
						Build context directory
					</label>
					<input
						id={contextDirId}
						className={`field-input ${changedFields.has("source.buildRecipe.contextDir") ? "unapplied-field" : ""}`}
						value={draft.contextDir}
						onChange={(e) =>
							setDraft((current) => ({
								...current,
								contextDir: e.target.value,
							}))
						}
						placeholder="."
					/>
				</div>
			</PanelSection>

			<ReplicaScaleControls
				service={service}
				onQueued={onSaved}
				onSavingChange={reportReplicaSaving}
			/>

			<PanelSection
				title="Placement"
				lede="Optionally keep this service in one operator-defined region. Replicas still spread across failure domains when capacity permits."
			>
				<div>
					<label className="field-label" htmlFor={placementRegionId}>
						Required region
					</label>
					<input
						id={placementRegionId}
						className={`field-input ${changedFields.has("placementRegion") ? "unapplied-field" : ""}`}
						value={draft.placementRegion}
						onChange={(event) =>
							setDraft((current) => ({
								...current,
								placementRegion: event.target.value,
							}))
						}
						placeholder="Any region"
					/>
				</div>
			</PanelSection>

			<PanelSection
				title="Rolling deployment"
				lede="How quickly healthy replacements enter service and old processes shut down."
			>
				<div
					className={`field-grid ${changedFields.has("rollingStrategy") ? "unapplied-field" : ""}`}
				>
					<RollingNumberField
						id={`service-rollout-unavailable-${service.id}`}
						label="Max unavailable"
						value={draft.maxUnavailable}
						onChange={(maxUnavailable) =>
							setDraft((current) => ({ ...current, maxUnavailable }))
						}
					/>
					<RollingNumberField
						id={`service-rollout-surge-${service.id}`}
						label="Max surge"
						value={draft.maxSurge}
						onChange={(maxSurge) =>
							setDraft((current) => ({ ...current, maxSurge }))
						}
					/>
					<RollingNumberField
						id={`service-rollout-startup-${service.id}`}
						label="Startup deadline (seconds)"
						value={draft.startupTimeoutSeconds}
						onChange={(startupTimeoutSeconds) =>
							setDraft((current) => ({ ...current, startupTimeoutSeconds }))
						}
					/>
					<RollingNumberField
						id={`service-rollout-drain-${service.id}`}
						label="Drain deadline (seconds)"
						value={draft.drainTimeoutSeconds}
						onChange={(drainTimeoutSeconds) =>
							setDraft((current) => ({ ...current, drainTimeoutSeconds }))
						}
					/>
				</div>
				<p className="field-hint">
					Healthy replacements enter ingress before old allocations receive
					SIGTERM. Remaining processes are force-killed only after the drain
					deadline.
				</p>
			</PanelSection>

			<PanelSection
				title="Process restart"
				lede="What happens when the process exits."
			>
				<fieldset
					className={`choice-rail ${
						changedFields.has("runtime.restart") ? "unapplied-field" : ""
					}`}
				>
					<legend className="field-label">Policy</legend>
					<label
						className={draft.restartPolicy === "on-failure" ? "active" : ""}
					>
						<input
							type="radio"
							name={`service-restart-policy-${service.id}`}
							checked={draft.restartPolicy === "on-failure"}
							onChange={() =>
								setDraft((current) => ({
									...current,
									restartPolicy: "on-failure",
								}))
							}
						/>
						On failure
					</label>
					<label className={draft.restartPolicy === "always" ? "active" : ""}>
						<input
							type="radio"
							name={`service-restart-policy-${service.id}`}
							checked={draft.restartPolicy === "always"}
							onChange={() =>
								setDraft((current) => ({
									...current,
									restartPolicy: "always",
								}))
							}
						/>
						Always
					</label>
					<label className={draft.restartPolicy === "never" ? "active" : ""}>
						<input
							type="radio"
							name={`service-restart-policy-${service.id}`}
							checked={draft.restartPolicy === "never"}
							onChange={() =>
								setDraft((current) => ({
									...current,
									restartPolicy: "never",
								}))
							}
						/>
						Never
					</label>
				</fieldset>
				<div className="field-grid">
					<div>
						<label
							className="field-label"
							htmlFor={`service-restart-max-${service.id}`}
						>
							Max restarts
						</label>
						<input
							id={`service-restart-max-${service.id}`}
							className="field-input"
							value={draft.maxRestarts}
							onChange={(e) =>
								setDraft((current) => ({
									...current,
									maxRestarts: e.target.value,
								}))
							}
							inputMode="numeric"
						/>
					</div>
					<div>
						<label
							className="field-label"
							htmlFor={`service-restart-window-${service.id}`}
						>
							Retry window (seconds)
						</label>
						<input
							id={`service-restart-window-${service.id}`}
							className="field-input"
							value={draft.windowSeconds}
							onChange={(e) =>
								setDraft((current) => ({
									...current,
									windowSeconds: e.target.value,
								}))
							}
							inputMode="numeric"
						/>
					</div>
				</div>
			</PanelSection>

			{error && <p className="error-msg">{error}</p>}

			<PanelSection
				title="Danger zone"
				tone="danger"
				lede="Irreversible. The service and its deployments leave this environment."
			>
				<div className="danger-zone">
					<div className="danger-zone-row">
						<div>
							<strong>Delete this service</strong>
							<span>
								Removes {service.name} from this environment. This cannot be
								undone.
							</span>
						</div>
						<button
							type="button"
							className="btn-danger-outline"
							onClick={() => {
								setDeleteError(undefined);
								setConfirmingDelete(true);
							}}
						>
							<Trash2 size={13} />
							Delete service
						</button>
					</div>
				</div>
			</PanelSection>

			{confirmingDelete && (
				<ConfirmDeleteDialog
					title="Delete Service"
					name={service.name}
					busy={deleting}
					error={deleteError}
					description={
						<>
							You are <span className="danger-word">deleting</span> the service{" "}
							<strong>{service.name}</strong> from this environment.
						</>
					}
					onCancel={() => {
						if (deleting) return;
						setConfirmingDelete(false);
						setDeleteError(undefined);
					}}
					onConfirm={() => void handleDelete()}
				/>
			)}

			{pickingRepo && (
				<SourceRepositoryDialog
					repositories={state.repositories}
					repoSelector={draft.repoSelector}
					onClose={() => setPickingRepo(false)}
					onSelect={(selector) => {
						setDraft((current) => ({
							...current,
							repoSelector: selector,
						}));
						setPickingRepo(false);
					}}
				/>
			)}
		</div>
	);
}

function settingsDraftFromService(
	service: DashboardServiceRecord,
): SettingsDraft {
	const source = service.spec?.source;
	return {
		repoSelector: source?.repositorySelector ?? "",
		trackedRef: source?.trackedRef ?? "",
		dockerfilePath: source?.buildRecipe?.dockerfilePath ?? "",
		contextDir: source?.buildRecipe?.contextDir ?? ".",
		restartPolicy: service.spec?.runtime.restart?.policy ?? "on-failure",
		maxRestarts: String(service.spec?.runtime.restart?.maxRestarts ?? 5),
		windowSeconds: String(service.spec?.runtime.restart?.windowSeconds ?? 300),
		placementRegion: service.spec?.placementRegion ?? "",
		maxUnavailable: String(
			service.spec?.rollingStrategy?.maxUnavailable ?? 0,
		),
		maxSurge: String(service.spec?.rollingStrategy?.maxSurge ?? 1),
		startupTimeoutSeconds: String(
			service.spec?.rollingStrategy?.startupTimeoutSeconds ?? 300,
		),
		drainTimeoutSeconds: String(
			service.spec?.rollingStrategy?.drainTimeoutSeconds ?? 30,
		),
	};
}

function RollingNumberField({
	id,
	label,
	value,
	onChange,
}: {
	id: string;
	label: string;
	value: string;
	onChange: (value: string) => void;
}) {
	return (
		<div>
			<label className="field-label" htmlFor={id}>
				{label}
			</label>
			<input
				id={id}
				className="field-input"
				value={value}
				onChange={(event) => onChange(event.target.value)}
				inputMode="numeric"
			/>
		</div>
	);
}

function SourceRepositoryDialog({
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
	// A repo can be set by hand — e.g. before the GitHub App has been installed,
	// when the catalog is empty. A well-formed owner/repo is offered as a row.
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
		<ModalOverlay ariaLabel="Select source repository" onClose={onClose}>
			<div className="modal-card">
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
		</ModalOverlay>
	);
}
