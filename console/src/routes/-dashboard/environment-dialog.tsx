import { Loader2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

import { doCreateEnvironment, doDuplicateEnvironment } from "./server-fns";
import { formatError } from "./service-utils";
import { ModalOverlay } from "./ui";

export function EnvironmentDialog({
	state,
	onClose,
	onCreated,
}: {
	state: DashboardHomeState;
	onClose: () => void;
	onCreated: (environmentId: string) => void;
}) {
	const [name, setName] = useState("");
	const [mode, setMode] = useState<"duplicate" | "empty">(
		state.environments.length > 0 ? "duplicate" : "empty",
	);
	const [sourceId, setSourceId] = useState(
		state.environment?.id ?? state.environments[0]?.id ?? "",
	);
	const [copyVariables, setCopyVariables] = useState(true);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const nameInputRef = useRef<HTMLInputElement>(null);
	const project = state.project;
	const canDuplicate = state.environments.length > 0;

	useEffect(() => {
		nameInputRef.current?.focus();
	}, []);

	const submit = async () => {
		if (!project || busy || !name.trim()) return;
		setBusy(true);
		setError(undefined);
		try {
			const created =
				mode === "duplicate" && sourceId
					? await doDuplicateEnvironment({
							data: {
								sourceEnvironmentId: sourceId,
								name: name.trim(),
								copyVariables,
							},
						})
					: await doCreateEnvironment({
							data: { projectId: project.id, name: name.trim() },
						});
			setBusy(false);
			onCreated(created.id);
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<ModalOverlay
			ariaLabel="New environment"
			onClose={() => {
				if (!busy) onClose();
			}}
		>
			<form
				className="modal-card env-dialog"
				onSubmit={(event) => {
					event.preventDefault();
					void submit();
				}}
			>
				<div className="env-dialog-head">
					<h2>New Environment</h2>
					<button
						type="button"
						className="icon-btn"
						aria-label="Close"
						onClick={onClose}
						disabled={busy}
					>
						<X size={15} />
					</button>
				</div>

				<p className="env-dialog-lede">
					All the changes will be isolated from other environments — deploy,
					edit, and break things without touching production.
				</p>

				<input
					ref={nameInputRef}
					className="field-input env-dialog-name"
					value={name}
					onChange={(event) => setName(event.target.value)}
					placeholder="staging"
					aria-label="Environment name"
					autoComplete="off"
					spellCheck={false}
					disabled={busy}
				/>

				{canDuplicate && (
					<label
						className={`env-option ${mode === "duplicate" ? "selected" : ""}`}
					>
						<span className="env-option-head">
							<input
								type="radio"
								name="environment-mode"
								checked={mode === "duplicate"}
								onChange={() => setMode("duplicate")}
								disabled={busy}
							/>
							<span className="env-option-title">Duplicate Environment</span>
						</span>
						<select
							className="field-input env-option-select"
							value={sourceId}
							onChange={(event) => setSourceId(event.target.value)}
							onClick={(event) => event.stopPropagation()}
							aria-label="Source environment"
							disabled={busy || mode !== "duplicate"}
						>
							{state.environments.map((entry) => (
								<option key={entry.id} value={entry.id}>
									{entry.name}
								</option>
							))}
						</select>
						<span className="env-option-help">
							Copy all the services and configuration from an existing
							environment.
						</span>
						{mode === "duplicate" && (
							<span className="env-option-check">
								<input
									type="checkbox"
									checked={copyVariables}
									onChange={(event) => setCopyVariables(event.target.checked)}
									disabled={busy}
								/>
								Copy variables
								{copyVariables && (
									<span className="env-option-warning">
										Copied values may contain production credentials.
									</span>
								)}
							</span>
						)}
					</label>
				)}

				<label className={`env-option ${mode === "empty" ? "selected" : ""}`}>
					<span className="env-option-head">
						<input
							type="radio"
							name="environment-mode"
							checked={mode === "empty"}
							onChange={() => setMode("empty")}
							disabled={busy}
						/>
						<span className="env-option-title">Empty Environment</span>
					</span>
					<span className="env-option-help">
						An empty environment with no services or variables included.
					</span>
				</label>

				{error && <p className="error-msg">{error}</p>}

				<button
					type="submit"
					className="btn-primary env-dialog-submit"
					disabled={busy || !name.trim() || !project}
				>
					{busy && (
						<Loader2
							size={13}
							style={{ animation: "spin 1s linear infinite" }}
						/>
					)}
					{busy ? "Creating…" : "Create Environment"}
				</button>
			</form>
		</ModalOverlay>
	);
}
