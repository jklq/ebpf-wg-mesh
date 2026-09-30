import * as stylex from "@stylexjs/stylex";
import { Github, Pencil } from "lucide-react";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { PanelSection } from "#/components/ui/section";
import { BuilderOption } from "#/features/dashboard/service-panel/settings/settings-fields";
import type { SettingsEditor } from "#/features/dashboard/service-panel/settings/settings-model";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { colors, fonts, motion, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	repositoryRow: {
		display: "flex",
		alignItems: "center",
		gap: "0.625rem",
		borderStyle: "solid",
		borderWidth: "1px",
		paddingInline: space.md,
		paddingBlock: "0.625rem",
		color: colors.ink,
	},
	repositoryDefault: {
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
	},
	repositoryName: {
		minWidth: "0rem",
		flex: "1",
		overflow: "hidden",
		fontFamily: fonts.mono,
		fontSize: "13px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	changeRepositoryButton: {
		display: "inline-flex",
		flexShrink: "0",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		padding: space.xs,
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
		transitionProperty:
			"color, background-color, border-color, outline-color, text-decoration-color, fill, stroke",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
	},
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
});
export function SourceBuildSettings({
	service,
	editor,
	onPickRepository,
}: {
	service: DashboardServiceRecord;
	editor: SettingsEditor;
	onPickRepository: () => void;
}) {
	const { draft, changedFields, updateDraft } = editor;
	const trackedRefId = `service-tracked-ref-${service.id}`;
	const dockerfilePathId = `service-dockerfile-path-${service.id}`;
	const contextDirId = `service-context-dir-${service.id}`;
	return (
		<>
			{" "}
			<PanelSection title="Source">
				<div>
					<p {...stylex.props(fieldStyles.label)}>Source repo</p>
					<div
						{...stylex.props([
							styles.repositoryRow,
							changedFields.has("source.repositorySelector")
								? fieldStyles.unappliedSurface
								: styles.repositoryDefault,
						])}
						data-unapplied={
							changedFields.has("source.repositorySelector") || undefined
						}
					>
						<Github size={16} aria-hidden="true" />
						<span {...stylex.props(styles.repositoryName)}>
							{draft.repoSelector || "No repository selected"}
						</span>
						<button
							type="button"
							{...stylex.props(styles.changeRepositoryButton)}
							aria-label="Change source repository"
							title="Change source repository"
							onClick={onPickRepository}
						>
							<Pencil size={14} />
						</button>
					</div>
				</div>

				<div>
					<label {...stylex.props(fieldStyles.label)} htmlFor={trackedRefId}>
						Branch
					</label>
					<TextInput
						id={trackedRefId}
						unapplied={changedFields.has("source.trackedRef")}
						data-unapplied={changedFields.has("source.trackedRef") || undefined}
						value={draft.trackedRef}
						onChange={(e) => updateDraft({ trackedRef: e.target.value })}
						placeholder="main"
					/>
				</div>
			</PanelSection>
			<PanelSection title="Build">
				<fieldset
					{...stylex.props([
						styles.builderOptions,
						changedFields.has("source.buildRecipe.builder")
							? fieldStyles.unappliedSurface
							: styles.builderOptionsDefault,
					])}
					data-unapplied={
						changedFields.has("source.buildRecipe.builder") || undefined
					}
				>
					<legend {...stylex.props([fieldStyles.label, styles.builderLegend])}>
						Builder
					</legend>
					<BuilderOption
						name={`service-builder-${service.id}`}
						label="Railpack"
						checked={draft.builder === "railpack"}
						onSelect={() => updateDraft({ builder: "railpack" })}
					/>
					<BuilderOption
						name={`service-builder-${service.id}`}
						label="Dockerfile"
						checked={draft.builder === "dockerfile"}
						onSelect={() => updateDraft({ builder: "dockerfile" })}
					/>
				</fieldset>

				{draft.builder === "dockerfile" && (
					<div>
						<label
							{...stylex.props(fieldStyles.label)}
							htmlFor={dockerfilePathId}
						>
							Dockerfile path
						</label>
						<TextInput
							id={dockerfilePathId}
							unapplied={changedFields.has("source.buildRecipe.dockerfilePath")}
							data-unapplied={
								changedFields.has("source.buildRecipe.dockerfilePath") ||
								undefined
							}
							value={draft.dockerfilePath}
							onChange={(e) => updateDraft({ dockerfilePath: e.target.value })}
							placeholder="Dockerfile"
						/>
					</div>
				)}

				<div>
					<label {...stylex.props(fieldStyles.label)} htmlFor={contextDirId}>
						{draft.builder === "dockerfile"
							? "Build context directory"
							: "Application directory"}
					</label>
					<TextInput
						id={contextDirId}
						unapplied={changedFields.has("source.buildRecipe.contextDir")}
						data-unapplied={
							changedFields.has("source.buildRecipe.contextDir") || undefined
						}
						value={draft.contextDir}
						onChange={(e) => updateDraft({ contextDir: e.target.value })}
						placeholder="."
					/>
				</div>
			</PanelSection>
		</>
	);
}
