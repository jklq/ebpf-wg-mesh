import * as stylex from "@stylexjs/stylex";
import { Code2, List, Plus, Trash2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Button } from "#/components/ui/button";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import { PanelSection } from "#/components/ui/section";
import { enqueueServicePersist } from "#/hooks/use-auto-queued-persist";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { envNameError, envValueError } from "#/lib/dashboard/env-variables";
import { doUpdateService } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

type VariableRow = {
	id: string;
	key: string;
	value: string;
};

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
	const savedEnvKey = stableEnvKey(service.spec?.runtime?.env ?? {});
	const changedEnvKeys = changedRuntimeEnvKeys(service);
	const savedEnv = service.spec?.runtime?.env ?? {};
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
		<div {...stylex.props(styles.panel)}>
			<PanelSection
				title="Environment"
				lede="Values are encrypted at rest and ship with the next deploy. Highlighted rows are not live yet."
			>
				<div {...stylex.props(styles.editorToolbar)}>
					<fieldset {...stylex.props(styles.editorModes)}>
						<legend {...stylex.props(styles.modeLegend)}>
							Variable editor mode
						</legend>
						<button type="button" {...stylex.props(styles.fieldsMode)}>
							<List size={13} />
							Fields
						</button>
						<button
							type="button"
							{...stylex.props(styles.rawMode)}
							onClick={openRawEditor}
						>
							<Code2 size={13} />
							Raw
						</button>
					</fieldset>
					<Button type="button" variant="secondary" onClick={addRow}>
						<Plus size={13} />
						Add variable
					</Button>
				</div>

				<div {...stylex.props(styles.variableList)}>
					{rows.length === 0 ? (
						<div {...stylex.props(styles.emptyState)}>
							<strong {...stylex.props(styles.emptyTitle)}>
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
									{...stylex.props([
										styles.variableRow,
										isUnapplied
											? [fieldStyles.unappliedSurface, styles.pendingRow]
											: styles.savedRow,
									])}
									data-unapplied={isUnapplied || undefined}
									key={row.id}
								>
									<div {...stylex.props(styles.variableField)}>
										<label
											{...stylex.props(fieldStyles.label)}
											htmlFor={`${row.id}-key`}
										>
											Name
										</label>
										<TextInput
											id={`${row.id}-key`}
											unapplied={isUnapplied}
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
									<div {...stylex.props(styles.variableField)}>
										<label
											{...stylex.props(fieldStyles.label)}
											htmlFor={`${row.id}-value`}
										>
											Value
										</label>
										<TextInput
											id={`${row.id}-value`}
											unapplied={isUnapplied}
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
									<Button
										type="button"
										variant="panelIcon"
										styles={[styles.removeButton]}
										onClick={() => removeRow(row.id)}
										title="Remove variable"
										aria-label="Remove variable"
									>
										<Trash2 size={13} />
									</Button>
								</div>
							);
						})
					)}
				</div>
			</PanelSection>

			{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}

			{rawDialogOpen && (
				<Dialog label="Raw variables" onClose={() => setRawDialogOpen(false)}>
					<div {...stylex.props([dialogStyles.card, styles.rawDialog])}>
						<div {...stylex.props(styles.rawDialogHeader)}>
							<div>
								<h2 {...stylex.props(styles.rawTitle)}>Raw variables</h2>
								<p {...stylex.props(styles.rawDescription)}>
									Enter one NAME=value pair per line.
								</p>
							</div>
							<Button
								type="button"
								variant="icon"
								aria-label="Close raw variables"
								onClick={() => setRawDialogOpen(false)}
							>
								<X size={16} />
							</Button>
						</div>
						<label
							{...stylex.props([fieldStyles.label, styles.rawLabel])}
							htmlFor={`variables-raw-${service.id}`}
						>
							Raw variables
						</label>
						<textarea
							ref={rawEditorRef}
							id={`variables-raw-${service.id}`}
							{...stylex.props([fieldStyles.input, styles.rawEditor])}
							value={raw}
							onChange={(event) => {
								setRaw(event.target.value);
								setError(undefined);
							}}
							placeholder={"DATABASE_URL=postgres://...\nREDIS_URL=redis://..."}
							spellCheck={false}
						/>
						{error && (
							<p {...stylex.props([noticeStyles.error, styles.rawError])}>
								{error}
							</p>
						)}
						<div {...stylex.props(styles.rawActions)}>
							<Button
								type="button"
								variant="secondary"
								onClick={() => setRawDialogOpen(false)}
							>
								Cancel
							</Button>
							<Button
								type="button"
								variant="primary"
								onClick={updateRawVariables}
							>
								Update variables
							</Button>
						</div>
					</div>
				</Dialog>
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
		const validation = envNameError(key) ?? envValueError(key, row.value);
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
		const value = unquoteValue(body.slice(equalsIndex + 1).trim());
		const validation = envNameError(key) ?? envValueError(key, value);
		if (validation) {
			return { ok: false, message: `Line ${index + 1}: ${validation}` };
		}
		if (Object.hasOwn(env, key)) {
			return {
				ok: false,
				message: `Line ${index + 1}: duplicate environment variable ${key}`,
			};
		}
		env[key] = value;
	}
	return { ok: true, env };
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

const styles = stylex.create({
	panel: { display: "flex", flexDirection: "column", gap: space.lg },
	editorToolbar: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.md,
	},
	editorModes: {
		position: "relative",
		margin: "0rem",
		display: "inline-flex",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
		padding: "0rem",
	},
	modeLegend: {
		position: "absolute",
		width: "1px",
		height: "1px",
		padding: "0",
		margin: "-1px",
		overflow: "hidden",
		clipPath: "inset(50%)",
		whiteSpace: "nowrap",
		borderWidth: "0",
	},
	fieldsMode: {
		display: "inline-flex",
		minHeight: "30px",
		cursor: "pointer",
		alignItems: "center",
		gap: "0.375rem",
		borderStyle: "solid",
		borderWidth: "0px",
		borderRightStyle: { default: "solid", ":last-child": "solid" },
		borderRightWidth: { default: "1px", ":last-child": "0px" },
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		paddingInline: "0.625rem",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		letterSpacing: "0.08em",
		color: colors.ink,
		textTransform: "uppercase",
	},
	rawMode: {
		display: "inline-flex",
		minHeight: "30px",
		cursor: "pointer",
		alignItems: "center",
		gap: "0.375rem",
		borderStyle: "solid",
		borderWidth: "0px",
		borderRightStyle: { default: "solid", ":last-child": "solid" },
		borderRightWidth: { default: "1px", ":last-child": "0px" },
		borderColor: colors.line,
		backgroundColor: "transparent",
		paddingInline: "0.625rem",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		letterSpacing: "0.08em",
		color: colors.muted,
		textTransform: "uppercase",
	},
	variableList: { display: "flex", flexDirection: "column", gap: "0.625rem" },
	emptyState: {
		display: "flex",
		flexDirection: "column",
		alignItems: "flex-start",
		gap: "0.375rem",
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: "color-mix(in oklab, #000 16%, transparent)",
		paddingInline: "18px",
		paddingBlock: "22px",
		fontSize: "13px",
		color: colors.muted,
	},
	emptyTitle: {
		fontFamily: fonts.display,
		fontSize: "18px",
		fontWeight: "500",
		color: colors.ink,
	},
	variableRow: {
		display: "grid",
		gridTemplateColumns: {
			default: "minmax(120px,0.8fr) minmax(160px,1.2fr) 32px",
			"@media (width < 40rem)": "1fr 32px",
		},
		alignItems: "flex-end",
		gap: space.sm,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
	},
	pendingRow: { borderStyle: "solid", borderWidth: "1px" },
	savedRow: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "color-mix(in oklab, #000 16%, transparent)",
	},
	variableField: {
		gridColumn: { default: null, "@media (width < 40rem)": "1 / -1" },
	},
	removeButton: {
		height: "34px",
		width: "2rem",
		justifyContent: "center",
		gridColumnStart: { default: null, "@media (width < 40rem)": "2" },
	},
	rawDialog: { maxWidth: "680px" },
	rawDialogHeader: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.md,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		padding: space.lg,
	},
	rawTitle: {
		margin: "0rem",
		fontFamily: fonts.display,
		fontSize: "26px",
		fontWeight: "500",
		letterSpacing: "-0.025em",
		color: colors.ink,
	},
	rawDescription: {
		marginTop: space.xs,
		marginBottom: "0rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	rawLabel: { marginInline: space.lg, marginTop: space.lg },
	rawEditor: {
		marginInline: space.lg,
		marginBottom: space.lg,
		minHeight: "260px",
		width: "calc(100% - 32px)",
		resize: "vertical",
		paddingInline: "0.875rem",
		paddingBlock: space.md,
		lineHeight: "1.55",
	},
	rawError: { marginInline: space.lg },
	rawActions: {
		display: "flex",
		alignItems: "center",
		justifyContent: "flex-end",
		gap: space.md,
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: colors.line,
		padding: space.lg,
	},
});
