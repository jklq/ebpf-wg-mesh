import { Code2, List, Plus, Trash2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { cn } from "#/lib/cn";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import {
	btnPrimary,
	btnSecondary,
	errorMsg,
	fieldInput,
	fieldInputUnapplied,
	fieldLabel,
	iconBtn,
	modalCard,
	panelIconBtn,
	unappliedSurface,
} from "#/lib/ui-classes";

import { PanelSecrets } from "./panel-secrets";
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
		<div className="flex flex-col gap-4">
			<PanelSection
				title="Environment"
				lede="These values ship with the next deploy. Highlighted rows are not live yet."
			>
				<div className="flex items-center justify-between gap-3">
					<fieldset className="relative m-0 inline-flex border border-line bg-canvas p-0">
						<legend className="sr-only">Variable editor mode</legend>
						<button
							type="button"
							className="inline-flex min-h-[30px] cursor-pointer items-center gap-1.5 border-0 border-r border-line bg-surface-raised px-2.5 font-condensed text-[11px] font-bold tracking-[0.08em] text-ink uppercase last:border-r-0"
						>
							<List size={13} />
							Fields
						</button>
						<button
							type="button"
							className="inline-flex min-h-[30px] cursor-pointer items-center gap-1.5 border-0 border-r border-line bg-transparent px-2.5 font-condensed text-[11px] font-bold tracking-[0.08em] text-muted uppercase last:border-r-0"
							onClick={openRawEditor}
						>
							<Code2 size={13} />
							Raw
						</button>
					</fieldset>
					<button type="button" className={btnSecondary} onClick={addRow}>
						<Plus size={13} />
						Add variable
					</button>
				</div>

				<div className="flex flex-col gap-2.5">
					{rows.length === 0 ? (
						<div className="flex flex-col items-start gap-1.5 border border-dashed border-line-bright bg-black/16 px-[18px] py-[22px] text-[13px] text-muted">
							<strong className="font-display text-[18px] font-medium text-ink">
								No variables yet
							</strong>
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
									className={cn(
										"grid grid-cols-[minmax(120px,0.8fr)_minmax(160px,1.2fr)_32px] items-end gap-2 px-3 py-2.5 max-sm:grid-cols-[1fr_32px] max-sm:[&>div]:col-span-full max-sm:[&>button]:col-start-2",
										isUnapplied
											? cn(unappliedSurface, "border")
											: "border border-line bg-black/16",
									)}
									data-unapplied={isUnapplied || undefined}
									key={row.id}
								>
									<div>
										<label className={fieldLabel} htmlFor={`${row.id}-key`}>
											Name
										</label>
										<input
											id={`${row.id}-key`}
											className={isUnapplied ? fieldInputUnapplied : fieldInput}
											data-unapplied={isUnapplied || undefined}
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
										<label className={fieldLabel} htmlFor={`${row.id}-value`}>
											Value
										</label>
										<input
											id={`${row.id}-value`}
											className={isUnapplied ? fieldInputUnapplied : fieldInput}
											data-unapplied={isUnapplied || undefined}
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
										className={cn(panelIconBtn, "h-[34px] w-8 justify-center")}
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

			<PanelSecrets key={service.id} serviceId={service.id} />
			{error && <p className={errorMsg}>{error}</p>}

			{rawDialogOpen && (
				<ModalOverlay onClose={() => setRawDialogOpen(false)}>
					<div className={cn(modalCard, "max-w-[680px]")}>
						<div className="flex items-center justify-between gap-3 border-b border-line p-4">
							<div>
								<h2 className="m-0 font-display text-[26px] font-medium tracking-tight text-ink">
									Raw variables
								</h2>
								<p className="mt-1 mb-0 text-xs text-muted">
									Enter one NAME=value pair per line.
								</p>
							</div>
							<button
								type="button"
								className={iconBtn}
								aria-label="Close raw variables"
								onClick={() => setRawDialogOpen(false)}
							>
								<X size={16} />
							</button>
						</div>
						<label
							className={cn(fieldLabel, "mx-4 mt-4")}
							htmlFor={`variables-raw-${service.id}`}
						>
							Raw variables
						</label>
						<textarea
							ref={rawEditorRef}
							id={`variables-raw-${service.id}`}
							className={cn(
								fieldInput,
								"mx-4 mb-4 min-h-[260px] w-[calc(100%-32px)] resize-y px-3.5 py-3 leading-[1.55]",
							)}
							value={raw}
							onChange={(event) => {
								setRaw(event.target.value);
								setError(undefined);
							}}
							placeholder={"DATABASE_URL=postgres://...\nREDIS_URL=redis://..."}
							spellCheck={false}
						/>
						{error && <p className={cn(errorMsg, "mx-4")}>{error}</p>}
						<div className="flex items-center justify-end gap-3 border-t border-line p-4">
							<button
								type="button"
								className={btnSecondary}
								onClick={() => setRawDialogOpen(false)}
							>
								Cancel
							</button>
							<button
								type="button"
								className={btnPrimary}
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
