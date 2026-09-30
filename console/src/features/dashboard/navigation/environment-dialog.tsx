import * as stylex from "@stylexjs/stylex";
import { Button } from "#/components/ui/button";
import { dialogStyles } from "#/components/ui/dialog";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { Loader2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Dialog } from "#/components/ui/dialog";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import {
	doCreateEnvironment,
	doDuplicateEnvironment,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

const envOptionCard = stylex.create({
	root: {
		display: "flex",
		cursor: "pointer",
		flexDirection: "column",
		gap: space.sm,
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		padding: space.md,
		transitionProperty: "border-color,background-color",
		transitionTimingFunction: "cubic-bezier(0, 0, 0.2, 1)",
		transitionDuration: motion.fast,
	},
});
const envOptionIdle = stylex.create({
	root: {
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
		},
		backgroundColor: colors.surfaceRaised,
	},
});
const envOptionSelected = stylex.create({
	root: { borderColor: colors.accent, backgroundColor: colors.accentDim },
});

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
		<Dialog
			label="New environment"
			onClose={() => {
				if (!busy) onClose();
			}}
		>
			<form
				{...stylex.props([dialogStyles.card, styles.form])}
				onSubmit={(event) => {
					event.preventDefault();
					void submit();
				}}
			>
				<div {...stylex.props(styles.header)}>
					<h2 {...stylex.props(styles.title)}>New Environment</h2>
					<Button
						type="button"
						variant="icon"
						aria-label="Close"
						onClick={onClose}
						disabled={busy}
					>
						<X size={15} />
					</Button>
				</div>

				<p {...stylex.props(styles.description)}>
					All the changes will be isolated from other environments — deploy,
					edit, and break things without touching production.
				</p>

				<TextInput
					ref={nameInputRef}
					styles={[styles.nameInput]}
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
						{...stylex.props([
							envOptionCard.root,
							mode === "duplicate"
								? envOptionSelected.root
								: envOptionIdle.root,
						])}
					>
						<span {...stylex.props(styles.optionHeading)}>
							<input
								type="radio"
								name="environment-mode"
								{...stylex.props(styles.radioInput)}
								checked={mode === "duplicate"}
								onChange={() => setMode("duplicate")}
								disabled={busy}
							/>
							<span {...stylex.props(styles.optionTitle)}>
								Duplicate Environment
							</span>
						</span>
						<select
							{...stylex.props([fieldStyles.input, styles.sourceSelect])}
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
						<span {...stylex.props(styles.optionDescription)}>
							Copy all the services and configuration from an existing
							environment.
						</span>
						{mode === "duplicate" && (
							<span {...stylex.props(styles.copyVariablesOption)}>
								<input
									type="checkbox"
									{...stylex.props(styles.checkboxInput)}
									checked={copyVariables}
									onChange={(event) => setCopyVariables(event.target.checked)}
									disabled={busy}
								/>
								Copy variables
								{copyVariables && (
									<span {...stylex.props(styles.credentialsHint)}>
										Copied values may contain production credentials.
									</span>
								)}
							</span>
						)}
					</label>
				)}

				<label
					{...stylex.props([
						envOptionCard.root,
						mode === "empty" ? envOptionSelected.root : envOptionIdle.root,
					])}
				>
					<span {...stylex.props(styles.optionHeading)}>
						<input
							type="radio"
							name="environment-mode"
							{...stylex.props(styles.radioInput)}
							checked={mode === "empty"}
							onChange={() => setMode("empty")}
							disabled={busy}
						/>
						<span {...stylex.props(styles.optionTitle)}>Empty Environment</span>
					</span>
					<span {...stylex.props(styles.optionDescription)}>
						An empty environment with no services or variables included.
					</span>
				</label>

				{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}

				<Button
					type="submit"
					variant="primary"
					styles={[styles.createButton]}
					disabled={busy || !name.trim() || !project}
				>
					{busy && <Loader2 size={13} {...stylex.props(styles.spinner)} />}
					{busy ? "Creating…" : "Create Environment"}
				</Button>
			</form>
		</Dialog>
	);
}

const styles = stylex.create({
	form: {
		display: "flex",
		maxWidth: "460px",
		flexDirection: "column",
		gap: "0.875rem",
		padding: "18px",
	},
	header: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.md,
	},
	title: {
		margin: "0rem",
		fontFamily: fonts.condensed,
		fontSize: "22px",
		letterSpacing: "0.02em",
		color: colors.ink,
	},
	description: {
		marginTop: "-0.375rem",
		marginBottom: "0rem",
		fontSize: "0.75rem",
		lineHeight: "1.5",
		color: colors.muted,
	},
	nameInput: {
		fontFamily: fonts.sans,
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
	},
	optionHeading: { display: "flex", alignItems: "center", gap: space.sm },
	radioInput: {
		width: "0.875rem",
		height: "0.875rem",
		accentColor: colors.accent,
	},
	optionTitle: { fontSize: "13px", fontWeight: "600", color: colors.ink },
	sourceSelect: { cursor: "pointer", fontFamily: fonts.sans },
	optionDescription: {
		fontSize: "0.75rem",
		lineHeight: "1.5",
		color: colors.muted,
	},
	copyVariablesOption: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: "0.375rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	checkboxInput: { accentColor: colors.accent },
	credentialsHint: {
		flexBasis: "100%",
		fontSize: "11px",
		color: colors.building,
	},
	createButton: {
		width: "100%",
		justifyContent: "center",
		paddingBlock: "0.625rem",
	},
	spinner: { animation: `${spin} 1s linear infinite` },
});
