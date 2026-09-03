import { Github, Pencil, Trash2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { cn } from "#/lib/cn";
import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import {
	btnDangerOutline,
	errorMsg,
	fieldInput,
	fieldInputUnapplied,
	fieldLabel,
	modalCard,
	unappliedSurface,
} from "#/lib/ui-classes";

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
	healthcheckTimeoutSeconds: string;
	drainingSeconds: string;
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
						healthcheckTimeoutSeconds:
							Number(next.healthcheckTimeoutSeconds) || 0,
						drainingSeconds: Number(next.drainingSeconds) || 0,
					},
				},
			});
			onSaved(updated);
		},
	});
	const changedFields = settingsChangedFields(service, incoming, draft);

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
		<div className="flex flex-col gap-7">
			<PanelSection title="Source">
				<div>
					<p className={fieldLabel}>Source repo</p>
					<div
						className={cn(
							"flex items-center gap-2.5 border px-3 py-2.5 text-ink",
							changedFields.has("source.repositorySelector")
								? unappliedSurface
								: "border-line bg-surface-raised",
						)}
						data-unapplied={
							changedFields.has("source.repositorySelector") || undefined
						}
					>
						<Github size={16} aria-hidden="true" />
						<span className="min-w-0 flex-1 overflow-hidden font-mono text-[13px] text-ellipsis whitespace-nowrap">
							{draft.repoSelector || "No repository selected"}
						</span>
						<button
							type="button"
							className="inline-flex shrink-0 cursor-pointer items-center justify-center border-0 bg-transparent p-1 text-muted transition-colors duration-100 hover:text-ink"
							aria-label="Change source repository"
							title="Change source repository"
							onClick={() => setPickingRepo(true)}
						>
							<Pencil size={14} />
						</button>
					</div>
				</div>

				<div>
					<label className={fieldLabel} htmlFor={trackedRefId}>
						Branch
					</label>
					<input
						id={trackedRefId}
						className={
							changedFields.has("source.trackedRef")
								? fieldInputUnapplied
								: fieldInput
						}
						data-unapplied={changedFields.has("source.trackedRef") || undefined}
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

			<PanelSection title="Build">
				<div>
					<label className={fieldLabel} htmlFor={dockerfilePathId}>
						Dockerfile path
					</label>
					<input
						id={dockerfilePathId}
						className={
							changedFields.has("source.buildRecipe.dockerfilePath")
								? fieldInputUnapplied
								: fieldInput
						}
						data-unapplied={
							changedFields.has("source.buildRecipe.dockerfilePath") ||
							undefined
						}
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
					<label className={fieldLabel} htmlFor={contextDirId}>
						Build context directory
					</label>
					<input
						id={contextDirId}
						className={
							changedFields.has("source.buildRecipe.contextDir")
								? fieldInputUnapplied
								: fieldInput
						}
						data-unapplied={
							changedFields.has("source.buildRecipe.contextDir") || undefined
						}
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

			<PanelSection title="Placement">
				<div>
					<label className={fieldLabel} htmlFor={placementRegionId}>
						Required region
					</label>
					<input
						id={placementRegionId}
						className={
							changedFields.has("placementRegion")
								? fieldInputUnapplied
								: fieldInput
						}
						data-unapplied={changedFields.has("placementRegion") || undefined}
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

			<PanelSection title="Deployment">
				<div
					className={cn(
						"grid grid-cols-2 gap-3 max-sm:grid-cols-1",
						changedFields.has("rollingStrategy") && unappliedSurface,
					)}
					data-unapplied={changedFields.has("rollingStrategy") || undefined}
				>
					<RollingNumberField
						id={`service-healthcheck-timeout-${service.id}`}
						label="Healthcheck timeout (seconds)"
						value={draft.healthcheckTimeoutSeconds}
						onChange={(healthcheckTimeoutSeconds) =>
							setDraft((current) => ({
								...current,
								healthcheckTimeoutSeconds,
							}))
						}
					/>
					<RollingNumberField
						id={`service-draining-time-${service.id}`}
						label="Draining time (seconds)"
						value={draft.drainingSeconds}
						onChange={(drainingSeconds) =>
							setDraft((current) => ({ ...current, drainingSeconds }))
						}
					/>
				</div>
			</PanelSection>

			<PanelSection title="Process restart">
				<fieldset
					className={cn(
						"relative m-0 flex flex-wrap border p-0",
						changedFields.has("runtime.restart")
							? unappliedSurface
							: "border-line bg-canvas",
					)}
					data-unapplied={changedFields.has("runtime.restart") || undefined}
				>
					<legend
						className={cn(
							fieldLabel,
							"absolute -top-2.5 mb-0 bg-canvas px-1.5",
						)}
					>
						Policy
					</legend>
					<label
						className={cn(
							"flex min-h-9 flex-1 cursor-pointer items-center justify-center border-r border-line px-3 font-condensed text-xs font-bold tracking-[0.08em] uppercase last:border-r-0",
							draft.restartPolicy === "on-failure"
								? "bg-surface-hover text-ink shadow-[inset_0_-2px_0_var(--color-accent)]"
								: "text-muted",
						)}
					>
						<input
							type="radio"
							className="pointer-events-none absolute opacity-0"
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
					<label
						className={cn(
							"flex min-h-9 flex-1 cursor-pointer items-center justify-center border-r border-line px-3 font-condensed text-xs font-bold tracking-[0.08em] uppercase last:border-r-0",
							draft.restartPolicy === "always"
								? "bg-surface-hover text-ink shadow-[inset_0_-2px_0_var(--color-accent)]"
								: "text-muted",
						)}
					>
						<input
							type="radio"
							className="pointer-events-none absolute opacity-0"
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
					<label
						className={cn(
							"flex min-h-9 flex-1 cursor-pointer items-center justify-center border-r border-line px-3 font-condensed text-xs font-bold tracking-[0.08em] uppercase last:border-r-0",
							draft.restartPolicy === "never"
								? "bg-surface-hover text-ink shadow-[inset_0_-2px_0_var(--color-accent)]"
								: "text-muted",
						)}
					>
						<input
							type="radio"
							className="pointer-events-none absolute opacity-0"
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
				<div className="grid grid-cols-2 gap-3 max-sm:grid-cols-1">
					<div>
						<label
							className={fieldLabel}
							htmlFor={`service-restart-max-${service.id}`}
						>
							Max restarts
						</label>
						<input
							id={`service-restart-max-${service.id}`}
							className={fieldInput}
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
							className={fieldLabel}
							htmlFor={`service-restart-window-${service.id}`}
						>
							Retry window (seconds)
						</label>
						<input
							id={`service-restart-window-${service.id}`}
							className={fieldInput}
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

			{error && <p className={errorMsg}>{error}</p>}

			<PanelSection title="Danger zone" tone="danger">
				<div>
					<div className="flex items-center justify-between gap-3.5 max-sm:flex-col max-sm:items-start">
						<div>
							<strong className="mb-[3px] block text-[13px] font-semibold text-ink">
								Delete this service
							</strong>
							<span className="block text-xs leading-normal text-muted">
								Removes {service.name} from this environment. This cannot be
								undone.
							</span>
						</div>
						<button
							type="button"
							className={btnDangerOutline}
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
							You are <span className="text-failed">deleting</span> the service{" "}
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

function settingsChangedFields(
	service: DashboardServiceRecord,
	incoming: SettingsDraft,
	draft: SettingsDraft,
): Set<string> {
	const changed = new Set(
		(service.unappliedChanges ?? []).map((change) => change.id),
	);
	if (draft.repoSelector !== incoming.repoSelector) {
		changed.add("source.repositorySelector");
	}
	if (draft.trackedRef !== incoming.trackedRef) {
		changed.add("source.trackedRef");
	}
	if (draft.dockerfilePath !== incoming.dockerfilePath) {
		changed.add("source.buildRecipe.dockerfilePath");
	}
	if (draft.contextDir !== incoming.contextDir) {
		changed.add("source.buildRecipe.contextDir");
	}
	if (draft.placementRegion !== incoming.placementRegion) {
		changed.add("placementRegion");
	}
	if (
		draft.healthcheckTimeoutSeconds !== incoming.healthcheckTimeoutSeconds ||
		draft.drainingSeconds !== incoming.drainingSeconds
	) {
		changed.add("rollingStrategy");
	}
	if (
		draft.restartPolicy !== incoming.restartPolicy ||
		draft.maxRestarts !== incoming.maxRestarts ||
		draft.windowSeconds !== incoming.windowSeconds
	) {
		changed.add("runtime.restart");
	}
	return changed;
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
		healthcheckTimeoutSeconds: String(
			service.spec?.rollingStrategy?.healthcheckTimeoutSeconds ?? 300,
		),
		drainingSeconds: String(
			service.spec?.rollingStrategy?.drainingSeconds ?? 30,
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
			<label className={fieldLabel} htmlFor={id}>
				{label}
			</label>
			<input
				id={id}
				className={fieldInput}
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
			<div className={modalCard}>
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
