import { Code2, List, Loader2, Plus, Save, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";

import type {
	DashboardProject,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { doUpdateService } from "./server-fns";
import { formatError } from "./service-utils";

type VariableMode = "fields" | "raw";

type VariableRow = {
	id: string;
	key: string;
	value: string;
};

const ENV_KEY_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function PanelVariables({
	service,
	project,
	onSaved,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject;
	onSaved: (service: DashboardServiceRecord) => void;
}) {
	const savedEnvKey = stableEnvKey(service.spec?.runtime.env ?? {});
	const changedEnvKeys = changedRuntimeEnvKeys(service);
	const [mode, setMode] = useState<VariableMode>("fields");
	const [rows, setRows] = useState<VariableRow[]>(() =>
		rowsFromStableEnvKey(savedEnvKey),
	);
	const [raw, setRaw] = useState(() =>
		formatRows(rowsFromStableEnvKey(savedEnvKey)),
	);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const [success, setSuccess] = useState(false);

	useEffect(() => {
		const nextRows = rowsFromStableEnvKey(savedEnvKey);
		setRows(nextRows);
		setRaw(formatRows(nextRows));
		setError(undefined);
		setSuccess(false);
		setMode("fields");
	}, [savedEnvKey]);

	const addRow = () => {
		setRows((current) => [...current, { id: nextRowId(), key: "", value: "" }]);
		setError(undefined);
		setSuccess(false);
	};

	const updateRow = (id: string, field: "key" | "value", value: string) => {
		setRows((current) =>
			current.map((row) => (row.id === id ? { ...row, [field]: value } : row)),
		);
		setError(undefined);
		setSuccess(false);
	};

	const removeRow = (id: string) => {
		setRows((current) => current.filter((row) => row.id !== id));
		setError(undefined);
		setSuccess(false);
	};

	const switchMode = (nextMode: VariableMode) => {
		if (nextMode === mode) {
			return;
		}
		setError(undefined);
		setSuccess(false);
		if (nextMode === "raw") {
			setRaw(formatRows(rows));
			setMode("raw");
			return;
		}
		const parsed = parseRawEnv(raw);
		if (!parsed.ok) {
			setError(parsed.message);
			return;
		}
		setRows(rowsFromEnv(parsed.env));
		setMode("fields");
	};

	const handleSave = async () => {
		setError(undefined);
		setSuccess(false);
		const parsed = mode === "raw" ? parseRawEnv(raw) : envFromRows(rows);
		if (!parsed.ok) {
			setError(parsed.message);
			return;
		}
		setSaving(true);
		try {
			const updated = await doUpdateService({
				data: {
					projectId: project.id,
					serviceId: service.id,
					runtimeEnv: parsed.env,
				},
			});
			const nextRows = rowsFromEnv(updated.spec?.runtime.env ?? {});
			setRows(nextRows);
			setRaw(formatRows(nextRows));
			setSuccess(true);
			onSaved(updated);
		} catch (e) {
			setError(formatError(e));
		} finally {
			setSaving(false);
		}
	};

	return (
		<div className="variables-panel">
			<div className="variables-toolbar">
				<fieldset className="variables-mode-toggle">
					<legend>Variable editor mode</legend>
					<button
						type="button"
						className={mode === "fields" ? "active" : ""}
						onClick={() => switchMode("fields")}
					>
						<List size={13} />
						Fields
					</button>
					<button
						type="button"
						className={mode === "raw" ? "active" : ""}
						onClick={() => switchMode("raw")}
					>
						<Code2 size={13} />
						Raw
					</button>
				</fieldset>
				{mode === "fields" && (
					<button type="button" className="btn-secondary" onClick={addRow}>
						<Plus size={13} />
						Add variable
					</button>
				)}
			</div>

			{mode === "fields" ? (
				<div className="variables-list">
					{rows.length === 0 ? (
						<div className="variables-empty">
							<span>No variables configured.</span>
						</div>
					) : (
						rows.map((row) => {
							const isUnapplied = changedEnvKeys.has(row.key);
							return (
							<div
								className={`variable-row ${isUnapplied ? "unapplied-field" : ""}`}
								key={row.id}
							>
								<div>
									<label className="field-label" htmlFor={`${row.id}-key`}>
										Name
									</label>
									<input
										id={`${row.id}-key`}
										className={`field-input ${isUnapplied ? "unapplied-field" : ""}`}
										value={row.key}
										onChange={(event) =>
											updateRow(row.id, "key", event.target.value)
										}
										placeholder="DATABASE_URL"
										spellCheck={false}
									/>
								</div>
								<div>
									<label className="field-label" htmlFor={`${row.id}-value`}>
										Value
									</label>
									<input
										id={`${row.id}-value`}
										className={`field-input ${isUnapplied ? "unapplied-field" : ""}`}
										value={row.value}
										onChange={(event) =>
											updateRow(row.id, "value", event.target.value)
										}
										placeholder="value"
										spellCheck={false}
									/>
								</div>
								<button
									type="button"
									className="panel-icon-btn variable-remove"
									onClick={() => removeRow(row.id)}
									title="Remove variable"
									aria-label="Remove variable"
								>
									<Trash2 size={13} />
								</button>
							</div>
							);
						})
					)}
				</div>
			) : (
				<div>
					<label
						className="field-label"
						htmlFor={`variables-raw-${service.id}`}
					>
						Raw variables
					</label>
					<textarea
						id={`variables-raw-${service.id}`}
						className={`field-input variables-raw-editor ${changedEnvKeys.size > 0 ? "unapplied-field" : ""}`}
						value={raw}
						onChange={(event) => {
							setRaw(event.target.value);
							setError(undefined);
							setSuccess(false);
						}}
						placeholder={"DATABASE_URL=postgres://...\nREDIS_URL=redis://..."}
						spellCheck={false}
					/>
				</div>
			)}

			{error && <p className="error-msg">{error}</p>}
			{success && (
				<p className="success-msg">
					Variables saved. Deploy the pending changes when ready.
				</p>
			)}

			<button
				type="button"
				className="btn-primary"
				onClick={handleSave}
				disabled={saving}
				style={{ alignSelf: "flex-start" }}
			>
				{saving ? (
					<Loader2 size={13} style={{ animation: "spin 1s linear infinite" }} />
				) : (
					<Save size={13} />
				)}
				{saving ? "Saving..." : "Save variables"}
			</button>
		</div>
	);
}

function changedRuntimeEnvKeys(service: DashboardServiceRecord): Set<string> {
	const prefix = "runtime.env.";
	return new Set(
		(service.unappliedChanges ?? [])
			.map((change) => change.id)
			.filter((id) => id.startsWith(prefix))
			.map((id) => id.slice(prefix.length)),
	);
}

function rowsFromEnv(env: Record<string, string>): VariableRow[] {
	return Object.entries(env)
		.sort(([left], [right]) => left.localeCompare(right))
		.map(([key, value]) => ({ id: nextRowId(), key, value }));
}

function envFromRows(
	rows: VariableRow[],
): { ok: true; env: Record<string, string> } | { ok: false; message: string } {
	const env: Record<string, string> = {};
	for (const row of rows) {
		const key = row.key.trim();
		if (key === "" && row.value === "") {
			continue;
		}
		const validation = validateKey(key);
		if (validation) {
			return { ok: false, message: validation };
		}
		if (Object.hasOwn(env, key)) {
			return { ok: false, message: `Duplicate environment variable: ${key}` };
		}
		env[key] = row.value;
	}
	return { ok: true, env };
}

function parseRawEnv(
	raw: string,
): { ok: true; env: Record<string, string> } | { ok: false; message: string } {
	const env: Record<string, string> = {};
	const lines = raw.split(/\r?\n/);
	for (let index = 0; index < lines.length; index++) {
		const original = lines[index];
		const line = original.trim();
		if (line === "" || line.startsWith("#")) {
			continue;
		}
		const body = line.startsWith("export ")
			? line.slice("export ".length)
			: line;
		const equalsIndex = body.indexOf("=");
		if (equalsIndex <= 0) {
			return {
				ok: false,
				message: `Line ${index + 1} must use NAME=value syntax.`,
			};
		}
		const key = body.slice(0, equalsIndex).trim();
		const validation = validateKey(key);
		if (validation) {
			return { ok: false, message: `Line ${index + 1}: ${validation}` };
		}
		if (Object.hasOwn(env, key)) {
			return {
				ok: false,
				message: `Line ${index + 1}: duplicate environment variable ${key}`,
			};
		}
		env[key] = unquoteValue(body.slice(equalsIndex + 1).trim());
	}
	return { ok: true, env };
}

function validateKey(key: string): string | undefined {
	if (key === "") {
		return "Environment variable name is required.";
	}
	if (!ENV_KEY_PATTERN.test(key)) {
		return `Invalid environment variable name: ${key}`;
	}
	return undefined;
}

function unquoteValue(value: string): string {
	if (
		(value.startsWith('"') && value.endsWith('"')) ||
		(value.startsWith("'") && value.endsWith("'"))
	) {
		return value.slice(1, -1);
	}
	return value;
}

function formatRows(rows: VariableRow[]): string {
	return rows
		.filter((row) => row.key.trim() !== "")
		.map((row) => `${row.key.trim()}=${row.value}`)
		.join("\n");
}

function stableEnvKey(env: Record<string, string>): string {
	return JSON.stringify(
		Object.entries(env).sort(([left], [right]) => left.localeCompare(right)),
	);
}

function rowsFromStableEnvKey(envKey: string): VariableRow[] {
	return (JSON.parse(envKey) as [string, string][]).map(([key, value]) => ({
		id: nextRowId(),
		key,
		value,
	}));
}

function nextRowId(): string {
	return `env-${Math.random().toString(36).slice(2)}`;
}
