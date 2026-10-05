import * as stylex from "@stylexjs/stylex";
import { Box } from "lucide-react";
import { useState } from "react";
import {
	PaletteList,
	PalettePrompt,
	paletteStyles,
} from "#/components/ui/command-palette";
import { Dialog } from "#/components/ui/dialog";
import {
	DEFAULT_VOLUME_MOUNT_PATH,
	mountPathError,
	serviceVolume,
} from "#/features/dashboard/volumes/volume-model";
import type {
	DashboardServiceRecord,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { doUpdateService } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

/** Services that can take a volume: none mounted in their draft yet. */
function mountableServices(
	services: Array<DashboardServiceRecord>,
	volumes: Array<DashboardVolume>,
) {
	return services.filter(
		(service) =>
			!service.spec?.runtime?.volume?.volumeName &&
			!serviceVolume(service, volumes) &&
			(service.spec?.desiredReplicaCount ?? 1) <= 1,
	);
}

/**
 * Two palette steps: pick the service, then type the mount path. Enter on the
 * path runs `onSubmit`; Backspace on an empty input walks back a step.
 */
export function VolumeMountFlow({
	crumb,
	services,
	volumes,
	onBack,
	onSubmit,
}: {
	crumb: string;
	services: Array<DashboardServiceRecord>;
	volumes: Array<DashboardVolume>;
	onBack?: () => void;
	onSubmit: (serviceId: string, mountPath: string) => Promise<void>;
}) {
	const candidates = mountableServices(services, volumes);
	const [service, setService] = useState<DashboardServiceRecord>();
	const [mountPath, setMountPath] = useState(DEFAULT_VOLUME_MOUNT_PATH);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();

	if (!service) {
		return (
			<PaletteList
				crumbs={[crumb]}
				placeholder="Service to attach volume to"
				items={candidates.map((entry) => ({
					id: entry.id,
					label: entry.name,
					icon: Box,
				}))}
				emptyText={
					candidates.length === 0
						? "Every service already mounts a volume or runs several replicas."
						: "No services match your search"
				}
				onPick={(item) =>
					setService(candidates.find((entry) => entry.id === item.id))
				}
				onBack={onBack}
			/>
		);
	}

	const submit = async () => {
		setBusy(true);
		setError(undefined);
		try {
			await onSubmit(service.id, mountPath.trim());
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<PalettePrompt
			crumbs={[crumb, "Volume mount path"]}
			label="Volume mount path"
			placeholder={DEFAULT_VOLUME_MOUNT_PATH}
			value={mountPath}
			busy={busy}
			inputError={mountPathError(mountPath)}
			error={error}
			hint={`Mounts on ${service.name} with the next deploy · Enter to confirm`}
			onChange={(value) => {
				setMountPath(value);
				setError(undefined);
			}}
			onSubmit={() => void submit()}
			onBack={() => setService(undefined)}
		/>
	);
}

export function MountVolumeDialog({
	volume,
	services,
	volumes,
	onClose,
	onServiceUpdated,
}: {
	volume: DashboardVolume;
	services: Array<DashboardServiceRecord>;
	volumes: Array<DashboardVolume>;
	onClose: () => void;
	onServiceUpdated: (service: DashboardServiceRecord) => void;
}) {
	return (
		<Dialog label={`Mount ${volume.name}`} onClose={onClose}>
			<div {...stylex.props(paletteStyles.card)}>
				<VolumeMountFlow
					crumb={`Mount ${volume.name}`}
					services={services}
					volumes={volumes}
					onBack={onClose}
					onSubmit={async (serviceId, mountPath) => {
						onServiceUpdated(
							await doUpdateService({
								data: {
									serviceId,
									volumeMount: { volumeName: volume.name, mountPath },
								},
							}),
						);
						onClose();
					}}
				/>
			</div>
		</Dialog>
	);
}
