import * as stylex from "@stylexjs/stylex";
import { useRouter } from "@tanstack/react-router";
import { History, Settings, Trash2 } from "lucide-react";
import { useState } from "react";
import { DeleteResourceDialog } from "#/components/delete-resource-dialog";
import { PageShell } from "#/components/layout/page-shell";
import { Button, buttonStyles } from "#/components/ui/button";
import { textStyles } from "#/components/ui/text";
import { LogRetentionForm } from "#/features/project-settings/log-retention";
import { SettingsCard } from "#/features/project-settings/settings-card";
import { VolumeRow } from "#/features/project-settings/volume-row";
import {
	doDeleteResource,
	fetchDeletionPreview,
	type loadProjectSettings,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

type PendingDelete =
	| { kind: "project"; id: string; name: string }
	| { kind: "volume"; id: string; name: string; environmentName: string };

export function ProjectSettingsPage({
	settings,
}: {
	settings: Awaited<ReturnType<typeof loadProjectSettings>> & { nowMs: number };
}) {
	const router = useRouter();

	const { project } = settings;
	const [pendingDelete, setPendingDelete] = useState<PendingDelete>();
	const [deleting, setDeleting] = useState(false);
	const [deleteError, setDeleteError] = useState<string>();
	const volumeCount = settings.environments.reduce(
		(total, entry) => total + entry.volumes.length,
		0,
	);

	const confirmDelete = async (confirmationName: string) => {
		if (!pendingDelete) return;
		setDeleting(true);
		setDeleteError(undefined);
		try {
			await doDeleteResource({
				data: {
					kind: pendingDelete.kind,
					id: pendingDelete.id,
					confirmationName,
				},
			});
			setPendingDelete(undefined);
			if (pendingDelete.kind === "project") {
				await router.navigate({ to: "/deleted" });
				return;
			}
			await router.invalidate();
		} catch (cause) {
			setDeleteError(formatError(cause));
		} finally {
			setDeleting(false);
		}
	};

	return (
		<PageShell
			title="Project settings"
			icon={<Settings size={15} />}
			back={{ href: `/projects/${project.id}`, label: project.name }}
			actions={
				<a
					href="/deleted"
					{...stylex.props([
						buttonStyles.base,
						buttonStyles.ghost,
						styles.recoveryLink,
					])}
				>
					<History size={13} /> Recently deleted
				</a>
			}
		>
			<div>
				<p {...stylex.props(textStyles.eyebrow)}>Project</p>
				<h1 {...stylex.props(styles.title)}>{project.name}</h1>
				<p {...stylex.props(styles.projectId)}>{project.id}</p>
			</div>

			<SettingsCard
				title="Log retention"
				lede="How long runtime, build, and deploy logs are kept for every service in this project."
			>
				<LogRetentionForm project={project} />
			</SettingsCard>

			<SettingsCard
				title="Volumes"
				lede="Persistent disks attached to services. Deleting a volume destroys its data when the grace period ends; volumes cannot be restored."
			>
				{volumeCount === 0 ? (
					<p {...stylex.props(styles.volumesEmpty)}>
						No volumes in this project.
					</p>
				) : (
					<div {...stylex.props(styles.volumeGroups)}>
						{settings.environments
							.filter((entry) => entry.volumes.length > 0)
							.map(({ environment, volumes }) => (
								<div key={environment.id} {...stylex.props(styles.volumeGroup)}>
									<span {...stylex.props(styles.environmentLabel)}>
										{environment.name}
										{environment.isProduction && (
											<span {...stylex.props(styles.productionBadge)}>
												prod
											</span>
										)}
									</span>
									<ul {...stylex.props(styles.volumeList)}>
										{volumes.map((volume) => (
											<VolumeRow
												nowMs={settings.nowMs}
												key={volume.id}
												volume={volume}
												onDelete={() => {
													setDeleteError(undefined);
													setPendingDelete({
														kind: "volume",
														id: volume.id,
														name: volume.name,
														environmentName: environment.name,
													});
												}}
											/>
										))}
									</ul>
								</div>
							))}
					</div>
				)}
			</SettingsCard>

			<SettingsCard title="Danger zone" tone="danger">
				<div {...stylex.props(styles.dangerActions)}>
					<div>
						<strong {...stylex.props(styles.dangerTitle)}>
							Delete this project
						</strong>
						<span {...stylex.props(styles.dangerDescription)}>
							Stops every service in every environment and removes their
							domains. You can restore the project from Recently deleted during
							the grace period.
						</span>
					</div>
					<Button
						type="button"
						variant="dangerOutline"
						onClick={() => {
							setDeleteError(undefined);
							setPendingDelete({
								kind: "project",
								id: project.id,
								name: project.name,
							});
						}}
					>
						<Trash2 size={13} />
						Delete project
					</Button>
				</div>
			</SettingsCard>

			{pendingDelete?.kind === "project" && (
				<DeleteResourceDialog
					title="Delete Project"
					name={pendingDelete.name}
					recovery="restorable"
					requireName
					loadPreview={() =>
						fetchDeletionPreview({
							data: { kind: "project", id: pendingDelete.id },
						})
					}
					busy={deleting}
					error={deleteError}
					description={
						<>
							You are{" "}
							<span {...stylex.props(styles.dangerEmphasis)}>deleting</span> the
							project <strong>{pendingDelete.name}</strong>. Everything in it
							stops serving immediately.
						</>
					}
					onCancel={() => {
						if (deleting) return;
						setPendingDelete(undefined);
					}}
					onConfirm={(confirmationName) => void confirmDelete(confirmationName)}
				/>
			)}
			{pendingDelete?.kind === "volume" && (
				<DeleteResourceDialog
					title="Delete Volume"
					name={pendingDelete.name}
					recovery="permanent"
					requireName
					previewServicesLabel="Mounted by"
					loadPreview={() =>
						fetchDeletionPreview({
							data: { kind: "volume", id: pendingDelete.id },
						})
					}
					busy={deleting}
					error={deleteError}
					description={
						<>
							You are{" "}
							<span {...stylex.props(styles.dangerEmphasis)}>deleting</span> the
							volume <strong>{pendingDelete.name}</strong> in{" "}
							{pendingDelete.environmentName}. A volume that a service still
							mounts cannot be deleted.
						</>
					}
					onCancel={() => {
						if (deleting) return;
						setPendingDelete(undefined);
					}}
					onConfirm={(confirmationName) => void confirmDelete(confirmationName)}
				/>
			)}
		</PageShell>
	);
}

const styles = stylex.create({
	recoveryLink: {
		gap: space.xs,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
	},
	title: {
		marginTop: "0.125rem",
		marginBottom: "5px",
		fontFamily: fonts.display,
		fontSize: "32px",
		fontWeight: "500",
	},
	projectId: {
		margin: "0rem",
		fontFamily: fonts.mono,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.dim,
	},
	volumesEmpty: {
		margin: "0rem",
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.lg,
		paddingBlock: "1.25rem",
		textAlign: "center",
		fontSize: "13px",
		color: colors.muted,
	},
	volumeGroups: { display: "flex", flexDirection: "column", gap: space.lg },
	volumeGroup: { display: "flex", flexDirection: "column", gap: space.sm },
	environmentLabel: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		letterSpacing: "0.1em",
		color: colors.muted,
		textTransform: "uppercase",
	},
	productionBadge: {
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.accentGlow,
		backgroundColor: colors.accentDim,
		paddingInline: "5px",
		paddingBlock: "1px",
		fontSize: "9px",
		color: colors.accent,
	},
	volumeList: {
		margin: "0rem",
		display: "flex",
		listStyleType: "none",
		flexDirection: "column",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		padding: "0rem",
	},
	dangerActions: {
		display: "flex",
		alignItems: { default: "center", "@media (width < 40rem)": "flex-start" },
		justifyContent: "space-between",
		gap: space.lg,
		flexDirection: { default: null, "@media (width < 40rem)": "column" },
	},
	dangerTitle: {
		marginBottom: "3px",
		display: "block",
		fontSize: "13px",
		fontWeight: "600",
		color: colors.ink,
	},
	dangerDescription: {
		display: "block",
		fontSize: "0.75rem",
		lineHeight: "1.5",
		color: colors.muted,
	},
	dangerEmphasis: { color: colors.failed },
});
