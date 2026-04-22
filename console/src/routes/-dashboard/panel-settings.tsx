import { Loader2 } from "lucide-react";
import { useState } from "react";

import type {
	DashboardHomeState,
	DashboardProject,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { doUpdateService } from "./server-fns";
import { formatError } from "./service-utils";

export function PanelSettings({
	service,
	project,
	state,
	onSaved,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject;
	state: DashboardHomeState;
	onSaved: () => void;
}) {
	const spec = service.spec;
	const [repoSelector, setRepoSelector] = useState(
		spec?.repositorySelector ?? "",
	);
	const [trackedRef, setTrackedRef] = useState(spec?.trackedRef ?? "");
	const [dockerfilePath, setDockerfilePath] = useState(
		spec?.buildRecipe?.dockerfilePath ?? "",
	);
	const [contextDir, setContextDir] = useState(
		spec?.buildRecipe?.contextDir ?? ".",
	);
	const [containerPort, setContainerPort] = useState(
		spec?.containerPort ? String(spec.containerPort) : "8080",
	);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const [success, setSuccess] = useState(false);

	const handleSave = async () => {
		setError(undefined);
		setSuccess(false);
		setSaving(true);
		try {
			await doUpdateService({
				data: {
					projectId: project.id,
					serviceId: service.id,
					repositorySelector: repoSelector,
					trackedRef,
					dockerfilePath,
					contextDir,
					containerPort,
				},
			});
			setSuccess(true);
			onSaved();
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
							<label className="field-label">Repository (from GitHub)</label>
							<select
								className="field-input"
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
						<label className="field-label">Repository (owner/repo)</label>
						<input
							className="field-input"
							value={repoSelector}
							onChange={(e) => setRepoSelector(e.target.value)}
							placeholder="owner/repo"
						/>
					</div>

					<div>
						<label className="field-label">Branch</label>
						<input
							className="field-input"
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
						<label className="field-label">Dockerfile path</label>
						<input
							className="field-input"
							value={dockerfilePath}
							onChange={(e) => setDockerfilePath(e.target.value)}
							placeholder="Dockerfile"
						/>
					</div>

					<div>
						<label className="field-label">Build context directory</label>
						<input
							className="field-input"
							value={contextDir}
							onChange={(e) => setContextDir(e.target.value)}
							placeholder="."
						/>
					</div>
				</div>
			</div>

			<div>
				<p className="section-header">Runtime</p>
				<div>
					<label className="field-label">Container port</label>
					<input
						className="field-input"
						value={containerPort}
						onChange={(e) => setContainerPort(e.target.value)}
						placeholder="8080"
						inputMode="numeric"
					/>
				</div>
			</div>

			{error && <p className="error-msg">{error}</p>}
			{success && (
				<p className="success-msg">
					Settings saved. A new build will start shortly.
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
				{saving ? "Saving…" : "Save & redeploy"}
			</button>
		</div>
	);
}
