import { Loader2 } from "lucide-react";
import { useState } from "react";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "#/lib/dashboard/core/defaults";

import { doUpdateService } from "./server-fns";
import { formatError } from "./service-utils";

export function PanelSettings({
	service,
	state,
	onSaved,
}: {
	service: DashboardServiceRecord;
	state: DashboardHomeState;
	onSaved: (service: DashboardServiceRecord) => void;
}) {
	const source = service.spec?.source;
	const changedFields = new Set(
		(service.unappliedChanges ?? []).map((change) => change.id),
	);
	const repoSelectId = `service-repo-select-${service.id}`;
	const repoInputId = `service-repo-input-${service.id}`;
	const trackedRefId = `service-tracked-ref-${service.id}`;
	const dockerfilePathId = `service-dockerfile-path-${service.id}`;
	const contextDirId = `service-context-dir-${service.id}`;
	const cpuMillisId = `service-cpu-millis-${service.id}`;
	const memoryMebibytesId = `service-memory-mebibytes-${service.id}`;
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
	const [cpuMillis, setCpuMillis] = useState(
		service.spec?.runtime.cpuMillis ?? DEFAULT_SERVICE_CPU_MILLIS,
	);
	const [memoryMebibytes, setMemoryMebibytes] = useState(
		service.spec?.runtime.memoryMebibytes ?? DEFAULT_SERVICE_MEMORY_MEBIBYTES,
	);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const [success, setSuccess] = useState(false);

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
					cpuMillis,
					memoryMebibytes,
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

	return (
		<div style={{ display: "flex", flexDirection: "column", gap: 20 }}>
			<div>
				<p className="section-header">Source</p>
				<div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
					{state.repositories.length > 0 && (
						<div>
							<label className="field-label" htmlFor={repoSelectId}>
								Repository (from GitHub)
							</label>
							<select
								id={repoSelectId}
								className={`field-input ${changedFields.has("source.repositorySelector") ? "unapplied-field" : ""}`}
								value={
									state.repositories.some((r) => r.fullName === repoSelector)
										? repoSelector
										: ""
								}
								onChange={(e) => {
									if (e.target.value) setRepoSelector(e.target.value);
								}}
							>
								<option value="">Choose a repo…</option>
								{state.repositories.map((r) => (
									<option key={r.fullName} value={r.fullName}>
										{r.fullName}
									</option>
								))}
							</select>
						</div>
					)}

					<div>
						<label className="field-label" htmlFor={repoInputId}>
							Repository (owner/repo)
						</label>
						<input
							id={repoInputId}
							className={`field-input ${changedFields.has("source.repositorySelector") ? "unapplied-field" : ""}`}
							value={repoSelector}
							onChange={(e) => setRepoSelector(e.target.value)}
							placeholder="owner/repo"
						/>
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

			<div>
				<p className="section-header">Resources</p>
				<div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
					<div>
						<label className="field-label" htmlFor={cpuMillisId}>
							CPU request (millicores)
						</label>
						<input
							id={cpuMillisId}
							type="number"
							min={DEFAULT_SERVICE_CPU_MILLIS}
							step={1}
							className={`field-input ${changedFields.has("runtime.cpuMillis") ? "unapplied-field" : ""}`}
							value={cpuMillis}
							onChange={(event) =>
								setCpuMillis(event.currentTarget.valueAsNumber)
							}
						/>
					</div>
					<div>
						<label className="field-label" htmlFor={memoryMebibytesId}>
							Memory request (MiB)
						</label>
						<input
							id={memoryMebibytesId}
							type="number"
							min={DEFAULT_SERVICE_MEMORY_MEBIBYTES}
							step={1}
							className={`field-input ${changedFields.has("runtime.memoryMebibytes") ? "unapplied-field" : ""}`}
							value={memoryMebibytes}
							onChange={(event) =>
								setMemoryMebibytes(event.currentTarget.valueAsNumber)
							}
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
				disabled={
					saving ||
					repoSelector.trim() === "" ||
					!Number.isSafeInteger(cpuMillis) ||
					cpuMillis < DEFAULT_SERVICE_CPU_MILLIS ||
					!Number.isSafeInteger(memoryMebibytes) ||
					memoryMebibytes < DEFAULT_SERVICE_MEMORY_MEBIBYTES
				}
				style={{ alignSelf: "flex-start" }}
			>
				{saving ? (
					<Loader2 size={13} style={{ animation: "spin 1s linear infinite" }} />
				) : null}
				{saving ? "Saving…" : "Save changes"}
			</button>
		</div>
	);
}
