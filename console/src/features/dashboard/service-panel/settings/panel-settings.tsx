import * as stylex from "@stylexjs/stylex";
import { Trash2 } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { DeleteResourceDialog } from "#/components/delete-resource-dialog";
import { Button } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";
import { PanelSection } from "#/components/ui/section";
import { ReplicaScaleControls } from "#/features/dashboard/service-panel/settings/replica-scale";
import { RuntimeSettings } from "#/features/dashboard/service-panel/settings/runtime-settings";
import type { SettingsDraft } from "#/features/dashboard/service-panel/settings/settings-model";
import {
	builderToProto,
	restartPolicyFromDraft,
	settingsChangedFields,
	settingsDraftFromService,
} from "#/features/dashboard/service-panel/settings/settings-model";
import { SourceBuildSettings } from "#/features/dashboard/service-panel/settings/source-build-settings";
import { SourceRepositoryDialog } from "#/features/dashboard/service-panel/settings/source-repository-dialog";
import { VolumeSettings } from "#/features/dashboard/service-panel/settings/volume-settings";
import { useAutoQueuedPersist } from "#/hooks/use-auto-queued-persist";
import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import {
	doDeleteService,
	doUpdateService,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors } from "#/styles/tokens.stylex";

export function PanelSettings({
	service,
	state,
	onSaved,
	onDeleted,
	onSavingChange,
}: {
	service: DashboardServiceRecord;
	state: DashboardHomeState;
	onSaved: (service: DashboardServiceRecord) => void;
	onDeleted: (serviceId: string) => void;
	onSavingChange?: (key: string, saving: boolean) => void;
}) {
	const incoming = settingsDraftFromService(service);
	const [confirmingDelete, setConfirmingDelete] = useState(false);
	const [deleting, setDeleting] = useState(false);
	const [deleteError, setDeleteError] = useState<string>();
	const [pickingRepo, setPickingRepo] = useState(false);
	const reportReplicaSaving = useCallback(
		(saving: boolean) => onSavingChange?.("replicas", saving),
		[onSavingChange],
	);

	const { draft, setDraft, error, saving } = useAutoQueuedPersist({
		serviceId: service.id,
		incoming,
		incomingEpoch: service.specRevision ?? 0,
		enabled: (next) => next.repoSelector.trim() !== "",
		persist: async (next) => {
			const updated = await doUpdateService({
				data: {
					serviceId: service.id,
					repositorySelector: next.repoSelector,
					trackedRef: next.trackedRef,
					builder: builderToProto(next.builder),
					dockerfilePath: next.dockerfilePath,
					contextDir: next.contextDir,
					restart: {
						policy: restartPolicyFromDraft(next.restartPolicy),
						maxRestarts: Number(next.maxRestarts) || 0,
						windowSeconds: Number(next.windowSeconds) || 0,
					},
					placementRegion: next.placementRegion,
					rollingStrategy: {
						healthcheckTimeoutSeconds:
							Number(next.healthcheckTimeoutSeconds) || 0,
						drainingSeconds: Number(next.drainingSeconds) || 0,
					},
				},
			});
			onSaved(updated);
		},
	});
	const updateDraft = (patch: Partial<SettingsDraft>) =>
		setDraft((current) => ({ ...current, ...patch }));
	const changedFields = settingsChangedFields(service, incoming, draft);

	const editor = { draft, changedFields, updateDraft };
	useEffect(() => {
		onSavingChange?.("settings", saving);
		return () => onSavingChange?.("settings", false);
	}, [onSavingChange, saving]);

	const handleDelete = async () => {
		setDeleting(true);
		setDeleteError(undefined);
		try {
			await doDeleteService({ data: { serviceId: service.id } });
			setConfirmingDelete(false);
			onDeleted(service.id);
		} catch (e) {
			setDeleteError(formatError(e));
			setDeleting(false);
		}
	};

	return (
		<div {...stylex.props(styles.sections)}>
			<SourceBuildSettings
				service={service}
				editor={editor}
				onPickRepository={() => setPickingRepo(true)}
			/>
			<ReplicaScaleControls
				service={service}
				onQueued={onSaved}
				onSavingChange={reportReplicaSaving}
			/>

			<VolumeSettings
				service={service}
				services={state.services}
				volumes={state.volumes}
				onSaved={onSaved}
			/>

			<RuntimeSettings service={service} editor={editor} />
			{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}

			<PanelSection title="Danger zone" tone="danger">
				<div>
					<div {...stylex.props(styles.dangerActions)}>
						<div>
							<strong {...stylex.props(styles.dangerTitle)}>
								Delete this service
							</strong>
							<span {...stylex.props(styles.dangerDescription)}>
								Stops {service.name} and removes it from this environment. You
								can restore it from Recently deleted.
							</span>
						</div>
						<Button
							type="button"
							variant="dangerOutline"
							onClick={() => {
								setDeleteError(undefined);
								setConfirmingDelete(true);
							}}
						>
							<Trash2 size={13} />
							Delete service
						</Button>
					</div>
				</div>
			</PanelSection>

			{confirmingDelete && (
				<DeleteResourceDialog
					title="Delete Service"
					name={service.name}
					recovery="restorable"
					requireName={false}
					busy={deleting}
					error={deleteError}
					description={
						<>
							You are{" "}
							<span {...stylex.props(styles.dangerEmphasis)}>deleting</span> the
							service <strong>{service.name}</strong> from this environment.
						</>
					}
					onCancel={() => {
						if (deleting) return;
						setConfirmingDelete(false);
						setDeleteError(undefined);
					}}
					onConfirm={() => void handleDelete()}
				/>
			)}

			{pickingRepo && (
				<SourceRepositoryDialog
					repositories={state.repositories}
					repoSelector={draft.repoSelector}
					onClose={() => setPickingRepo(false)}
					onSelect={(selector) => {
						updateDraft({ repoSelector: selector });
						setPickingRepo(false);
					}}
				/>
			)}
		</div>
	);
}

const styles = stylex.create({
	sections: { display: "flex", flexDirection: "column", gap: "1.75rem" },
	dangerActions: {
		display: "flex",
		alignItems: { default: "center", "@media (width < 40rem)": "flex-start" },
		justifyContent: "space-between",
		gap: "0.875rem",
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
