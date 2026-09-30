import * as stylex from "@stylexjs/stylex";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { PanelSection } from "#/components/ui/section";
import { RollingNumberField } from "#/features/dashboard/service-panel/settings/settings-fields";
import type { SettingsEditor } from "#/features/dashboard/service-panel/settings/settings-model";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	builderOptions: {
		position: "relative",
		margin: "0rem",
		display: "flex",
		flexWrap: "wrap",
		borderStyle: "solid",
		borderWidth: "1px",
		padding: "0rem",
	},
	builderOptionsDefault: {
		borderColor: colors.line,
		backgroundColor: colors.canvas,
	},
	builderLegend: {
		position: "absolute",
		top: "-0.625rem",
		marginBottom: "0rem",
		backgroundColor: colors.canvas,
		paddingInline: "0.375rem",
	},
	fieldGrid: {
		display: "grid",
		gridTemplateColumns: {
			default: "repeat(2, minmax(0, 1fr))",
			"@media (width < 40rem)": "repeat(1, minmax(0, 1fr))",
		},
		gap: space.md,
	},
	restartOption: {
		display: "flex",
		minHeight: "2.25rem",
		flex: "1",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderRightStyle: { default: "solid", ":last-child": "solid" },
		borderRightWidth: { default: "1px", ":last-child": "0px" },
		borderColor: colors.line,
		paddingInline: space.md,
		fontFamily: fonts.condensed,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		fontWeight: "700",
		letterSpacing: "0.08em",
		textTransform: "uppercase",
	},
	selectedRestartOption: {
		backgroundColor: colors.surfaceHover,
		color: colors.ink,
		boxShadow: `inset 0 -2px 0 ${colors.accent}`,
	},
	idleRestartOption: { color: colors.muted },
	radioInput: { pointerEvents: "none", position: "absolute", opacity: "0%" },
});
export function RuntimeSettings({
	service,
	editor,
}: {
	service: DashboardServiceRecord;
	editor: SettingsEditor;
}) {
	const { draft, changedFields, updateDraft } = editor;
	const placementRegionId = `service-placement-region-${service.id}`;
	return (
		<>
			{" "}
			<PanelSection title="Placement">
				<div>
					<label
						{...stylex.props(fieldStyles.label)}
						htmlFor={placementRegionId}
					>
						Required region
					</label>
					<TextInput
						id={placementRegionId}
						unapplied={changedFields.has("placementRegion")}
						data-unapplied={changedFields.has("placementRegion") || undefined}
						value={draft.placementRegion}
						onChange={(event) =>
							updateDraft({ placementRegion: event.target.value })
						}
						placeholder="Any region"
					/>
				</div>
			</PanelSection>
			<PanelSection title="Deployment">
				<div
					{...stylex.props([
						styles.fieldGrid,
						changedFields.has("rollingStrategy") &&
							fieldStyles.unappliedSurface,
					])}
					data-unapplied={changedFields.has("rollingStrategy") || undefined}
				>
					<RollingNumberField
						id={`service-healthcheck-timeout-${service.id}`}
						label="Healthcheck timeout (seconds)"
						value={draft.healthcheckTimeoutSeconds}
						onChange={(healthcheckTimeoutSeconds) =>
							updateDraft({ healthcheckTimeoutSeconds })
						}
					/>
					<RollingNumberField
						id={`service-draining-time-${service.id}`}
						label="Draining time (seconds)"
						value={draft.drainingSeconds}
						onChange={(drainingSeconds) => updateDraft({ drainingSeconds })}
					/>
				</div>
			</PanelSection>
			<PanelSection title="Process restart">
				<fieldset
					{...stylex.props([
						styles.builderOptions,
						changedFields.has("runtime.restart")
							? fieldStyles.unappliedSurface
							: styles.builderOptionsDefault,
					])}
					data-unapplied={changedFields.has("runtime.restart") || undefined}
				>
					<legend {...stylex.props([fieldStyles.label, styles.builderLegend])}>
						Policy
					</legend>
					<label
						{...stylex.props([
							styles.restartOption,
							draft.restartPolicy === "on-failure"
								? styles.selectedRestartOption
								: styles.idleRestartOption,
						])}
					>
						<input
							type="radio"
							{...stylex.props(styles.radioInput)}
							name={`service-restart-policy-${service.id}`}
							checked={draft.restartPolicy === "on-failure"}
							onChange={() => updateDraft({ restartPolicy: "on-failure" })}
						/>
						On failure
					</label>
					<label
						{...stylex.props([
							styles.restartOption,
							draft.restartPolicy === "always"
								? styles.selectedRestartOption
								: styles.idleRestartOption,
						])}
					>
						<input
							type="radio"
							{...stylex.props(styles.radioInput)}
							name={`service-restart-policy-${service.id}`}
							checked={draft.restartPolicy === "always"}
							onChange={() => updateDraft({ restartPolicy: "always" })}
						/>
						Always
					</label>
					<label
						{...stylex.props([
							styles.restartOption,
							draft.restartPolicy === "never"
								? styles.selectedRestartOption
								: styles.idleRestartOption,
						])}
					>
						<input
							type="radio"
							{...stylex.props(styles.radioInput)}
							name={`service-restart-policy-${service.id}`}
							checked={draft.restartPolicy === "never"}
							onChange={() => updateDraft({ restartPolicy: "never" })}
						/>
						Never
					</label>
				</fieldset>
				<div {...stylex.props(styles.fieldGrid)}>
					<div>
						<label
							{...stylex.props(fieldStyles.label)}
							htmlFor={`service-restart-max-${service.id}`}
						>
							Max restarts
						</label>
						<TextInput
							id={`service-restart-max-${service.id}`}
							value={draft.maxRestarts}
							onChange={(e) => updateDraft({ maxRestarts: e.target.value })}
							inputMode="numeric"
						/>
					</div>
					<div>
						<label
							{...stylex.props(fieldStyles.label)}
							htmlFor={`service-restart-window-${service.id}`}
						>
							Retry window (seconds)
						</label>
						<TextInput
							id={`service-restart-window-${service.id}`}
							value={draft.windowSeconds}
							onChange={(e) => updateDraft({ windowSeconds: e.target.value })}
							inputMode="numeric"
						/>
					</div>
				</div>
			</PanelSection>
		</>
	);
}
