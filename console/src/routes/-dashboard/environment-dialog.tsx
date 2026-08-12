import { useState } from "react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import {
	doCreateEnvironment,
	doDeleteEnvironment,
	doDuplicateEnvironment,
	doRenameEnvironment,
} from "./server-fns";

export function EnvironmentDialog({
	state,
	onClose,
}: {
	state: DashboardHomeState;
	onClose: () => void;
}) {
	const [mode, setMode] = useState<
		"create" | "duplicate" | "rename" | "delete"
	>("create");
	const [name, setName] = useState("");
	const [copyVariables, setCopyVariables] = useState(false);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const environment = state.environment;
	const project = state.project;

	const submit = async () => {
		if (!environment || !project) return;
		setBusy(true);
		setError(undefined);
		try {
			if (mode === "create") {
				const created = await doCreateEnvironment({
					data: { projectId: project.id, name },
				});
				window.location.assign(
					`/environments/${encodeURIComponent(created.id)}`,
				);
				return;
			}
			if (mode === "duplicate") {
				const created = await doDuplicateEnvironment({
					data: { sourceEnvironmentId: environment.id, name, copyVariables },
				});
				window.location.assign(
					`/environments/${encodeURIComponent(created.id)}`,
				);
				return;
			}
			if (mode === "rename") {
				await doRenameEnvironment({
					data: { environmentId: environment.id, name },
				});
				window.location.reload();
				return;
			}
			await doDeleteEnvironment({ data: { environmentId: environment.id } });
			const fallback = state.environments.find((entry) => entry.isProduction);
			window.location.assign(
				fallback ? `/environments/${encodeURIComponent(fallback.id)}` : "/",
			);
		} catch (cause) {
			setError(
				cause instanceof Error ? cause.message : "Environment operation failed",
			);
			setBusy(false);
		}
	};

	return (
		<div
			style={{
				position: "fixed",
				inset: 0,
				zIndex: 80,
				display: "grid",
				placeItems: "center",
				background: "rgba(0,0,0,.55)",
			}}
		>
			<div
				role="dialog"
				aria-modal="true"
				aria-label="Manage environment"
				style={{
					width: 420,
					maxWidth: "calc(100vw - 32px)",
					padding: 18,
					background: "var(--surface)",
					border: "1px solid var(--border)",
					boxShadow: "0 20px 70px rgba(0,0,0,.5)",
				}}
			>
				<div style={{ display: "flex", gap: 6, marginBottom: 16 }}>
					{(["create", "duplicate", "rename", "delete"] as const).map(
						(value) => (
							<button
								key={value}
								type="button"
								className="btn-ghost"
								disabled={value === "delete" && environment?.isProduction}
								onClick={() => {
									setMode(value);
									setName(value === "rename" ? (environment?.name ?? "") : "");
								}}
							>
								{value}
							</button>
						),
					)}
				</div>
				{mode !== "delete" && (
					<input
						value={name}
						onChange={(event) => setName(event.target.value)}
						placeholder="Environment name"
						style={{ width: "100%" }}
					/>
				)}
				{mode === "duplicate" && (
					<label
						style={{
							display: "block",
							marginTop: 12,
							fontSize: 12,
							color: "var(--text-muted)",
						}}
					>
						<input
							type="checkbox"
							checked={copyVariables}
							onChange={(event) => setCopyVariables(event.target.checked)}
						/>{" "}
						Copy variables
						{copyVariables && (
							<span
								style={{
									display: "block",
									marginTop: 6,
									color: "var(--warning)",
								}}
							>
								Copied values may contain production credentials.
							</span>
						)}
					</label>
				)}
				{mode === "delete" && (
					<p>
						Delete <strong>{environment?.name}</strong> and all of its services
						and runtime state?
					</p>
				)}
				{error && (
					<p style={{ color: "var(--failed)", fontSize: 12 }}>{error}</p>
				)}
				<div
					style={{
						display: "flex",
						justifyContent: "flex-end",
						gap: 8,
						marginTop: 18,
					}}
				>
					<button type="button" className="btn-ghost" onClick={onClose}>
						Cancel
					</button>
					<button
						type="button"
						className="btn-primary"
						disabled={busy || (mode !== "delete" && !name.trim())}
						onClick={() => void submit()}
					>
						{busy ? "Working…" : mode}
					</button>
				</div>
			</div>
		</div>
	);
}
