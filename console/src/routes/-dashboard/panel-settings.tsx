import { Github, Loader2, Pencil, Trash2 } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { ConfirmDeleteDialog } from "./confirm-delete-dialog";
import { RepositoryPicker } from "./repository-picker";
import { doDeleteService, doUpdateService } from "./server-fns";
import { formatError } from "./service-utils";
import { ModalOverlay } from "./ui";

const MANUAL_SELECTOR = /^[\w.-]+\/[\w.-]+$/;

export function PanelSettings({
	service,
	state,
	onSaved,
	onDeleted,
}: {
	service: DashboardServiceRecord;
	state: DashboardHomeState;
	onSaved: (service: DashboardServiceRecord) => void;
	onDeleted: (serviceId: string) => void;
}) {
	const source = service.spec?.source;
	const changedFields = new Set(
		(service.unappliedChanges ?? []).map((change) => change.id),
	);
	const trackedRefId = `service-tracked-ref-${service.id}`;
	const dockerfilePathId = `service-dockerfile-path-${service.id}`;
	const contextDirId = `service-context-dir-${service.id}`;
	const [repoSelector, setRepoSelector] = useState(
		source?.repositorySelector ?? "",
	);
	const [trackedRef, setTrackedRef] = useState(source?.trackedRef ?? "");
	const [dockerfilePath, setDockerfilePath] = useState(
		source?.buildRecipe?.dockerfilePath ?? "",
	);
	const [contextDir, setContextDir] = useState(
		source?.buildRecipe?.contextDir ?? ".",
	);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const [success, setSuccess] = useState(false);
	const [pickingRepo, setPickingRepo] = useState(false);
	const [confirmingDelete, setConfirmingDelete] = useState(false);
	const [deleting, setDeleting] = useState(false);
	const [deleteError, setDeleteError] = useState<string>();

	const handleSave = async () => {
		setError(undefined);
		setSuccess(false);
		setSaving(true);
		try {
			const updated = await doUpdateService({
				data: {
					serviceId: service.id,
					repositorySelector: repoSelector,
					trackedRef,
					dockerfilePath,
					contextDir,
				},
			});
			setSuccess(true);
			onSaved(updated);
		} catch (e) {
			setError(formatError(e));
		} finally {
			setSaving(false);
		}
	};

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
		<div style={{ display: "flex", flexDirection: "column", gap: 20 }}>
			<div>
				<p className="section-header">Source</p>
				<div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
					<div>
						<p className="field-label">Source Repo</p>
						<div
							className={`source-repo-card ${changedFields.has("source.repositorySelector") ? "unapplied-field" : ""}`}
						>
							<Github size={16} aria-hidden="true" />
							<span className="source-repo-name">
								{repoSelector || "No repository selected"}
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
							value={trackedRef}
							onChange={(e) => setTrackedRef(e.target.value)}
							placeholder="main"
						/>
					</div>
				</div>
			</div>

			<div>
				<p className="section-header">Build</p>
				<div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
					<div>
						<label className="field-label" htmlFor={dockerfilePathId}>
							Dockerfile path
						</label>
						<input
							id={dockerfilePathId}
							className={`field-input ${changedFields.has("source.buildRecipe.dockerfilePath") ? "unapplied-field" : ""}`}
							value={dockerfilePath}
							onChange={(e) => setDockerfilePath(e.target.value)}
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
							value={contextDir}
							onChange={(e) => setContextDir(e.target.value)}
							placeholder="."
						/>
					</div>
				</div>
			</div>

			{error && <p className="error-msg">{error}</p>}
			{success && (
				<p className="success-msg">
					Settings saved. Deploy the pending changes when ready.
				</p>
			)}

			<button
				type="button"
				className="btn-primary"
				onClick={handleSave}
				disabled={saving || repoSelector.trim() === ""}
				style={{ alignSelf: "flex-start" }}
			>
				{saving ? (
					<Loader2 size={13} style={{ animation: "spin 1s linear infinite" }} />
				) : null}
				{saving ? "Saving…" : "Save changes"}
			</button>

			<div className="danger-zone">
				<p className="section-header danger">Danger zone</p>
				<div className="danger-zone-row">
					<div>
						<strong>Delete this service</strong>
						<span>
							Removes {service.name} and its deployments from this environment.
							This cannot be undone.
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
					repoSelector={repoSelector}
					onClose={() => setPickingRepo(false)}
					onSelect={(selector) => {
						setRepoSelector(selector);
						setPickingRepo(false);
					}}
				/>
			)}
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
