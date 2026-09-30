import * as stylex from "@stylexjs/stylex";
import { useId, useState } from "react";
import { Button } from "#/components/ui/button";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import {
	doCreateFleetAgent,
	doUpdateFleetAgent,
} from "#/features/fleet/server/functions";
import type {
	DashboardAgentEnrollment,
	DashboardFleetAgent,
	FleetAgentInput,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/errors";
import { safeInteger } from "#/lib/platform-json";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	card: { maxWidth: "min(620px, calc(100vw - 32px))", padding: "22px" },
	title: { marginTop: "0rem" },
	description: { color: colors.muted },
	fieldGrid: {
		marginBlock: "1.25rem",
		display: "grid",
		gridTemplateColumns: {
			default: "repeat(2, minmax(0, 1fr))",
			"@media (width < 40rem)": "repeat(1, minmax(0, 1fr))",
		},
		gap: space.md,
	},
	actions: {
		marginTop: "1.25rem",
		display: "flex",
		justifyContent: "flex-end",
		gap: space.sm,
	},
	bootstrapToken: {
		display: "block",
		overflowWrap: "anywhere",
		WebkitUserSelect: "all",
		userSelect: "all",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.accent,
		backgroundColor: colors.accentDim,
		padding: space.md,
		fontFamily: fonts.mono,
	},
});
export function FleetAgentDialog({
	agent,
	onClose,
	onSaved,
}: {
	agent?: DashboardFleetAgent;
	onClose: () => void;
	onSaved: (enrollment?: DashboardAgentEnrollment) => Promise<void>;
}) {
	const [draft, setDraft] = useState<FleetAgentInput>({
		agentId: agent?.id ?? "",
		name: agent?.name ?? "",
		region: agent?.region ?? "",
		zone: agent?.zone ?? "",
		failureDomain: agent?.failureDomain ?? "",
		reservedCpuMillis: safeInteger(agent?.reservedCpuMillis ?? "0"),
		reservedMemoryMebibytes: safeInteger(agent?.reservedMemoryMebibytes ?? "0"),
	});
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const save = async () => {
		setSaving(true);
		setError(undefined);
		try {
			if (agent) {
				await doUpdateFleetAgent({ data: draft });
				await onSaved();
			} else {
				await onSaved(await doCreateFleetAgent({ data: draft }));
			}
		} catch (cause) {
			setError(formatError(cause));
			setSaving(false);
		}
	};
	return (
		<Dialog
			label={agent ? "Edit fleet node" : "Enroll fleet node"}
			onClose={() => {
				if (!saving) onClose();
			}}
		>
			<div {...stylex.props([dialogStyles.card, styles.card])}>
				<h2 {...stylex.props(styles.title)}>
					{agent ? "Edit fleet node" : "Enroll fleet node"}
				</h2>
				<p {...stylex.props(styles.description)}>
					Topology and reservations are operator policy. The agent only reports
					observed host capacity and capabilities.
				</p>
				<div {...stylex.props(styles.fieldGrid)}>
					<FleetField
						label="Agent ID"
						value={draft.agentId}
						disabled={Boolean(agent)}
						onChange={(agentId) => setDraft({ ...draft, agentId })}
					/>
					<FleetField
						label="Name"
						value={draft.name}
						onChange={(name) => setDraft({ ...draft, name })}
					/>
					<FleetField
						label="Region"
						value={draft.region}
						onChange={(region) => setDraft({ ...draft, region })}
					/>
					<FleetField
						label="Zone (optional)"
						value={draft.zone}
						onChange={(zone) => setDraft({ ...draft, zone })}
					/>
					<FleetField
						label="Failure domain"
						value={draft.failureDomain}
						onChange={(failureDomain) => setDraft({ ...draft, failureDomain })}
					/>
					<FleetField
						label="Reserved CPU (mCPU)"
						type="number"
						value={String(draft.reservedCpuMillis)}
						onChange={(value) =>
							setDraft({ ...draft, reservedCpuMillis: Number(value) })
						}
					/>
					<FleetField
						label="Reserved memory (MiB)"
						type="number"
						value={String(draft.reservedMemoryMebibytes)}
						onChange={(value) =>
							setDraft({
								...draft,
								reservedMemoryMebibytes: Number(value),
							})
						}
					/>
				</div>
				{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}
				<div {...stylex.props(styles.actions)}>
					<Button
						type="button"
						variant="ghost"
						disabled={saving}
						onClick={onClose}
					>
						Cancel
					</Button>
					<Button
						type="button"
						variant="primary"
						disabled={saving}
						onClick={() => void save()}
					>
						{saving ? "Saving…" : agent ? "Save policy" : "Create enrollment"}
					</Button>
				</div>
			</div>
		</Dialog>
	);
}
export function FleetField({
	label,
	value,
	onChange,
	disabled,
	type = "text",
}: {
	label: string;
	value: string;
	onChange: (value: string) => void;
	disabled?: boolean;
	type?: "text" | "number";
}) {
	const id = useId();
	return (
		<label htmlFor={id}>
			<span {...stylex.props(fieldStyles.label)}>{label}</span>
			<TextInput
				id={id}
				type={type}
				min={type === "number" ? 0 : undefined}
				value={value}
				disabled={disabled}
				onChange={(event) => onChange(event.target.value)}
			/>
		</label>
	);
}
export function EnrollmentSecret({
	enrollment,
	onClose,
}: {
	enrollment: DashboardAgentEnrollment;
	onClose: () => void;
}) {
	return (
		<Dialog label="Agent bootstrap token" closeOnBackdrop={false}>
			<div {...stylex.props([dialogStyles.card, styles.card])}>
				<h2 {...stylex.props(styles.title)}>Enrollment created</h2>
				<p {...stylex.props(styles.description)}>
					Copy this single-use secret now. It will not be shown again.
				</p>
				<code {...stylex.props(styles.bootstrapToken)}>
					{enrollment.bootstrapToken}
				</code>
				<div {...stylex.props(styles.actions)}>
					<Button type="button" variant="primary" onClick={onClose}>
						I stored the token
					</Button>
				</div>
			</div>
		</Dialog>
	);
}
