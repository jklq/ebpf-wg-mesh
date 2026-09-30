type RetentionMode = "default" | "custom";

import * as stylex from "@stylexjs/stylex";
import { useRouter } from "@tanstack/react-router";
import { Check, Loader2 } from "lucide-react";
import { type FormEvent, type ReactNode, useEffect, useState } from "react";
import { Button } from "#/components/ui/button";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import type { DashboardProject } from "#/lib/dashboard/core/types.server";
import { doUpdateProjectLogRetention } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, motion, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
const styles = stylex.create({
	retentionForm: { display: "flex", flexDirection: "column", gap: "0.875rem" },
	retentionOptions: {
		margin: "0rem",
		display: "flex",
		flexDirection: "column",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "0px",
		padding: "0rem",
	},
	retentionLegend: { marginBottom: space.sm },
	customRetention: { display: "flex", alignItems: "center", gap: space.sm },
	retentionInput: { width: "5rem", textAlign: "right" },
	retentionUnit: { fontSize: "13px", color: colors.muted },
	retentionValidation: {
		margin: "0rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.failed,
	},
	retentionError: { margin: "0rem" },
	retentionActions: { display: "flex", alignItems: "center", gap: space.md },
	spinner: { animation: `${spin} 1s linear infinite` },
	retentionSaved: {
		display: "inline-flex",
		alignItems: "center",
		gap: space.xs,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.healthy,
	},
	retentionOption: {
		display: "flex",
		alignItems: { default: "center", "@media (width < 40rem)": "flex-start" },
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		paddingInline: "0.875rem",
		paddingBlock: space.md,
		transitionProperty:
			"color, background-color, border-color, outline-color, text-decoration-color, fill, stroke",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
		flexDirection: { default: null, "@media (width < 40rem)": "column" },
	},
	selectedRetention: {
		borderColor: "rgba(226,138,36,0.5)",
		backgroundColor: colors.accentDim,
	},
	idleRetention: { borderColor: colors.line, backgroundColor: colors.canvas },
	retentionLabel: {
		display: "flex",
		flex: "1",
		cursor: "pointer",
		alignItems: "center",
		gap: space.md,
	},
	retentionRadio: {
		width: "0.875rem",
		height: "0.875rem",
		accentColor: colors.accent,
	},
	retentionCopy: { display: "flex", flexDirection: "column" },
	retentionName: { fontSize: "13px", fontWeight: "600", color: colors.ink },
	retentionDetail: {
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
});
export function LogRetentionForm({ project }: { project: DashboardProject }) {
	const router = useRouter();
	const saved = project.logRetentionDays ?? 0;
	const [mode, setMode] = useState<RetentionMode>(
		saved === 0 ? "default" : "custom",
	);
	const [days, setDays] = useState(saved === 0 ? "30" : String(saved));
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const [savedNotice, setSavedNotice] = useState(false);

	useEffect(() => {
		if (!savedNotice) return;
		const id = window.setTimeout(() => setSavedNotice(false), 2400);
		return () => window.clearTimeout(id);
	}, [savedNotice]);

	const parsedDays = /^\d+$/.test(days.trim())
		? Number(days.trim())
		: Number.NaN;
	const daysValid =
		Number.isInteger(parsedDays) && parsedDays >= 1 && parsedDays <= 90;
	const next = mode === "default" ? 0 : parsedDays;
	const dirty = mode === "default" ? saved !== 0 : parsedDays !== saved;
	const canSave = !saving && dirty && (mode === "default" || daysValid);

	const submit = async (event: FormEvent) => {
		event.preventDefault();
		if (!canSave) return;
		setSaving(true);
		setError(undefined);
		try {
			await doUpdateProjectLogRetention({
				data: { projectId: project.id, logRetentionDays: next },
			});
			setSavedNotice(true);
			await router.invalidate();
		} catch (cause) {
			// Keep the user's input so they can correct it.
			setError(formatError(cause));
		} finally {
			setSaving(false);
		}
	};

	return (
		<form {...stylex.props(styles.retentionForm)} onSubmit={submit}>
			<fieldset {...stylex.props(styles.retentionOptions)}>
				<legend {...stylex.props([fieldStyles.label, styles.retentionLegend])}>
					Keep logs for
				</legend>
				<RetentionOption
					checked={mode === "default"}
					label="Platform default"
					detail="Follows the platform's retention policy."
					onSelect={() => setMode("default")}
				/>
				<RetentionOption
					checked={mode === "custom"}
					label="Custom"
					detail="Between 1 and 90 days."
					onSelect={() => setMode("custom")}
				>
					<div {...stylex.props(styles.customRetention)}>
						<TextInput
							styles={[styles.retentionInput]}
							inputMode="numeric"
							value={days}
							aria-label="Retention in days"
							aria-invalid={mode === "custom" && !daysValid}
							disabled={mode !== "custom"}
							onChange={(event) => {
								setDays(event.target.value);
								setError(undefined);
							}}
						/>
						<span {...stylex.props(styles.retentionUnit)}>days</span>
					</div>
				</RetentionOption>
			</fieldset>

			{mode === "custom" && !daysValid && days.trim() !== "" && (
				<p {...stylex.props(styles.retentionValidation)}>
					Enter a whole number of days from 1 to 90.
				</p>
			)}
			{error && (
				<p
					{...stylex.props([noticeStyles.error, styles.retentionError])}
					role="alert"
				>
					{error}
				</p>
			)}

			<div {...stylex.props(styles.retentionActions)}>
				<Button type="submit" variant="primary" disabled={!canSave}>
					{saving && <Loader2 size={12} {...stylex.props(styles.spinner)} />}
					{saving ? "Saving…" : "Save retention"}
				</Button>
				{savedNotice && (
					<output {...stylex.props(styles.retentionSaved)}>
						<Check size={13} /> Saved
					</output>
				)}
			</div>
		</form>
	);
}
export function RetentionOption({
	checked,
	label,
	detail,
	onSelect,
	children,
}: {
	checked: boolean;
	label: string;
	detail: string;
	onSelect: () => void;
	children?: ReactNode;
}) {
	return (
		<div
			{...stylex.props([
				styles.retentionOption,
				checked ? styles.selectedRetention : styles.idleRetention,
			])}
		>
			<label {...stylex.props(styles.retentionLabel)}>
				<input
					type="radio"
					name="log-retention-mode"
					{...stylex.props(styles.retentionRadio)}
					checked={checked}
					onChange={onSelect}
				/>
				<span {...stylex.props(styles.retentionCopy)}>
					<span {...stylex.props(styles.retentionName)}>{label}</span>
					<span {...stylex.props(styles.retentionDetail)}>{detail}</span>
				</span>
			</label>
			{children}
		</div>
	);
}
