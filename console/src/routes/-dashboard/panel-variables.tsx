import { Code2, List, Plus, Trash2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

import { doUpdateService } from "./server-fns";
import { formatError } from "./service-utils";
import { ModalOverlay, PanelSection } from "./ui";
import { enqueueServicePersist } from "./use-auto-queued-persist";

type VariableRow = {
	id: string;
	key: string;
	value: string;
};

const ENV_KEY_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function PanelVariables({
	service,
	onSaved,
	seedKey,
	onSavingChange,
}: {
	service: DashboardServiceRecord;
	onSaved: (service: DashboardServiceRecord) => void;
	seedKey?: string;
	onSavingChange?: (saving: boolean) => void;
}) {
	const savedEnvKey = stableEnvKey(service.spec?.runtime.env ?? {});
	const changedEnvKeys = changedRuntimeEnvKeys(service);
	const savedEnv = service.spec?.runtime.env ?? {};
	const [rows, setRows] = useState<VariableRow[]>(() =>
		rowsFromStableEnvKey(savedEnvKey),
	);
	const [raw, setRaw] = useState(() =>
		formatRows(rowsFromStableEnvKey(savedEnvKey)),
	);
	const [rawDialogOpen, setRawDialogOpen] = useState(false);
	const [error, setError] = useState<string>();
	const [saving, setSaving] = useState(false);
	const [focusRowId, setFocusRowId] = useState<string>();
	const rawEditorRef = useRef<HTMLTextAreaElement>(null);
	const persistRef = useRef(onSaved);
	persistRef.current = onSaved;
	const savingChangeRef = useRef(onSavingChange);
	savingChangeRef.current = onSavingChange;
	const serviceRef = useRef(service);
	serviceRef.current = service;
	const savedEnvKeyRef = useRef(savedEnvKey);
	savedEnvKeyRef.current = savedEnvKey;
	const queuedEnvKeyRef = useRef(savedEnvKey);
	const localEditPendingRef = useRef(false);
	const persistGenerationRef = useRef(0);
	const seededKeyRef = useRef<string | undefined>(undefined);

	useEffect(() => {
		const incomingIsOurs = savedEnvKey === queuedEnvKeyRef.current;
		if (incomingIsOurs && seedKey === seededKeyRef.current) {
			localEditPendingRef.current = false;
			return;
		}
		if (localEditPendingRef.current || saving || rawDialogOpen) return;
		const nextRows = rowsFromStableEnvKey(savedEnvKey);
		let nextFocus: string | undefined;
		if (seedKey) {
			const existing = nextRows.find((row) => row.key === seedKey);
			if (existing) {
				nextFocus = existing.id;
			} else {
				const seeded = { id: nextRowId(), key: seedKey, value: "" };
				nextRows.push(seeded);
				nextFocus = seeded.id;
			}
		}
		setRows(nextRows);
		setRaw(formatRows(nextRows));
		setError(undefined);
		setFocusRowId(nextFocus);
		queuedEnvKeyRef.current = savedEnvKey;
		seededKeyRef.current = seedKey;
	}, [rawDialogOpen, savedEnvKey, saving, seedKey]);

	useEffect(() => {
		if (!focusRowId) return;
		document.getElementById(`${focusRowId}-value`)?.focus();
	}, [focusRowId]);

	useEffect(() => {
		if (rawDialogOpen) rawEditorRef.current?.focus();
	}, [rawDialogOpen]);

	useEffect(
		() => () => {
			savingChangeRef.current?.(false);
		},
		[],
	);

	const addRow = () => {
		setRows((current) => [...current, { id: nextRowId(), key: "", value: "" }]);
		setError(undefined);
	};

	const updateRow = (id: string, field: "key" | "value", value: string) => {
		setRows((current) =>
			current.map((row) => (row.id === id ? { ...row, [field]: value } : row)),
		);
		setError(undefined);
	};

	const removeRow = (id: string) => {
		const nextRows = rows.filter((row) => row.id !== id);
		setRows(nextRows);
		setError(undefined);
		commitRows(nextRows);
	};

	const persistEnv = (env: Record<string, string>) => {
		const nextKey = stableEnvKey(env);
		if (nextKey === queuedEnvKeyRef.current) {
			return;
		}
		queuedEnvKeyRef.current = nextKey;
		localEditPendingRef.current = true;
		const generation = persistGenerationRef.current + 1;
		persistGenerationRef.current = generation;
		setSaving(true);
		savingChangeRef.current?.(true);
		const serviceId = serviceRef.current.id;
		void enqueueServicePersist(serviceId, () =>
			doUpdateService({ data: { serviceId, runtimeEnv: env } }),
		)
			.then((updated) => {
				if (generation === persistGenerationRef.current) {
					persistRef.current(updated);
				}
			})
			.catch((cause) => {
				if (generation === persistGenerationRef.current) {
					queuedEnvKeyRef.current = savedEnvKeyRef.current;
					localEditPendingRef.current = false;
					setError(formatError(cause));
				}
			})
			.finally(() => {
				if (generation === persistGenerationRef.current) {
					setSaving(false);
					savingChangeRef.current?.(false);
				}
			});
	};

	const commitRows = (nextRows: VariableRow[] = rows) => {
		const parsed = envFromRows(nextRows);
		if (!parsed.ok) {
			setError(parsed.message);
			return;
		}
		setError(undefined);
		persistEnv(parsed.env);
	};

	const openRawEditor = () => {
		setRaw(formatRows(rows));
		setError(undefined);
		setRawDialogOpen(true);
	};

	const updateRawVariables = () => {
		const parsed = parseRawEnv(raw);
		if (!parsed.ok) {
			setError(parsed.message);
			return;
		}
		setRows(rowsFromEnv(parsed.env));
		setError(undefined);
		setRawDialogOpen(false);
		persistEnv(parsed.env);
	};

	return (
		<div className="variables-panel">
			<PanelSection
				title="Environment"
				lede="These values ship with the next deploy. Highlighted rows are not live yet."
			>
				<div className="variables-toolbar">
					<fieldset className="variables-mode-toggle">
						<legend>Variable editor mode</legend>
						<button type="button" className="active">
							<List size={13} />
							Fields
						</button>
						<button type="button" onClick={openRawEditor}>
							<Code2 size={13} />
							Raw
						</button>
					</fieldset>
					<button type="button" className="btn-secondary" onClick={addRow}>
						<Plus size={13} />
						Add variable
					</button>
				</div>

				<div className="variables-list">
					{rows.length === 0 ? (
						<div className="variables-empty">
							<strong>No variables yet</strong>
							<span>Add a name and value, or switch to raw KEY=value.</span>
						</div>
					) : (
						rows.map((row) => {
							const hasLocalChange =
								(row.key !== "" || row.value !== "") &&
								(!Object.hasOwn(savedEnv, row.key) ||
									savedEnv[row.key] !== row.value);
							const isUnapplied = changedEnvKeys.has(row.key) || hasLocalChange;
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
											onBlur={() => commitRows()}
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
											onBlur={() => commitRows()}
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
			</PanelSection>

			{error && <p className="error-msg">{error}</p>}

			{rawDialogOpen && (
				<ModalOverlay onClose={() => setRawDialogOpen(false)}>
					<div className="modal-card variables-raw-dialog">
						<div className="unapplied-dialog-header">
							<div>
								<h2>Raw variables</h2>
								<p>Enter one NAME=value pair per line.</p>
							</div>
							<button
								type="button"
								className="icon-btn"
								aria-label="Close raw variables"
								onClick={() => setRawDialogOpen(false)}
							>
								<X size={16} />
							</button>
						</div>
						<label
							className="field-label"
							htmlFor={`variables-raw-${service.id}`}
						>
							Raw variables
						</label>
						<textarea
							ref={rawEditorRef}
							id={`variables-raw-${service.id}`}
							className="field-input variables-raw-editor"
							value={raw}
							onChange={(event) => {
								setRaw(event.target.value);
								setError(undefined);
							}}
							placeholder={"DATABASE_URL=postgres://...\nREDIS_URL=redis://..."}
							spellCheck={false}
						/>
						{error && <p className="error-msg">{error}</p>}
						<div className="unapplied-dialog-footer">
							<button
								type="button"
								className="btn-secondary"
								onClick={() => setRawDialogOpen(false)}
							>
								Cancel
							</button>
							<button
								type="button"
								className="btn-primary"
								onClick={updateRawVariables}
							>
								Update variables
							</button>
						</div>
					</div>
				</ModalOverlay>
			)}
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
