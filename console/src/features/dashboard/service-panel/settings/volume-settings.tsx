import * as stylex from "@stylexjs/stylex";
import { HardDrive, Loader2, Unlink } from "lucide-react";
import { useId, useState } from "react";
import { Button } from "#/components/ui/button";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import { PanelSection } from "#/components/ui/section";
import { StatusDot } from "#/components/ui/status-dot";
import {
	DEFAULT_VOLUME_MOUNT_PATH,
	mountPathError,
	unattachedVolumes,
	volumeTone,
	volumeUsage,
} from "#/features/dashboard/volumes/volume-model";
import { formatBytes } from "#/lib/bytes";
import type {
	DashboardServiceRecord,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { doUpdateService } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

/** Attach, move, or detach the service's volume; every change is staged. */
export function VolumeSettings({
	service,
	services,
	volumes,
	onSaved,
}: {
	service: DashboardServiceRecord;
	services: Array<DashboardServiceRecord>;
	volumes: Array<DashboardVolume>;
	onSaved: (service: DashboardServiceRecord) => void;
}) {
	const mount = service.spec?.runtime?.volume;
	const mountedName = mount?.volumeName?.trim() ?? "";
	const mountedPath = mount?.mountPath?.trim() || DEFAULT_VOLUME_MOUNT_PATH;
	const mounted = volumes.find((volume) => volume.name === mountedName);
	const candidates = unattachedVolumes(volumes, services);
	const replicas = service.spec?.desiredReplicaCount ?? 1;
	const unapplied = (service.unappliedChanges ?? []).some(
		(change) => change.id === "runtime.volume",
	);

	const [volumeName, setVolumeName] = useState(candidates[0]?.name ?? "");
	const [path, setPath] = useState(mountedPath);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const selectId = useId();
	const pathId = useId();
	const pathError = mountPathError(path);
	const chosenName = candidates.some((volume) => volume.name === volumeName)
		? volumeName
		: (candidates[0]?.name ?? "");
	const targetName = mountedName || chosenName;
	const pathChanged = path.trim() !== mountedPath;

	const save = async (
		volumeMount: {
			volumeName: string;
			mountPath: string;
		} | null,
	) => {
		setBusy(true);
		setError(undefined);
		try {
			const updated = await doUpdateService({
				data: { serviceId: service.id, volumeMount },
			});
			onSaved(updated);
			setPath(
				updated.spec?.runtime?.volume?.mountPath || DEFAULT_VOLUME_MOUNT_PATH,
			);
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};

	return (
		<PanelSection title="Volume">
			<div
				{...stylex.props([
					styles.body,
					unapplied && [fieldStyles.unappliedSurface, styles.unapplied],
				])}
				data-unapplied={unapplied || undefined}
			>
				{mountedName ? (
					<div {...stylex.props(styles.mounted)}>
						<HardDrive size={14} {...stylex.props(styles.icon)} />
						<span {...stylex.props(styles.volumeName)}>{mountedName}</span>
						{mounted ? (
							<>
								<span {...stylex.props(styles.usage)}>
									{formatBytes(volumeUsage(mounted).used)} /{" "}
									{formatBytes(volumeUsage(mounted).size)}
								</span>
								<StatusDot health={volumeTone(mounted)} />
							</>
						) : (
							<span {...stylex.props(styles.usage)}>missing</span>
						)}
						<span {...stylex.props(styles.spacer)} />
						<Button
							type="button"
							variant="ghost"
							disabled={busy}
							onClick={() => void save(null)}
						>
							<Unlink size={13} />
							Detach
						</Button>
					</div>
				) : candidates.length > 0 ? (
					<div>
						<label {...stylex.props(fieldStyles.label)} htmlFor={selectId}>
							Volume
						</label>
						<select
							id={selectId}
							{...stylex.props([fieldStyles.input, styles.select])}
							value={chosenName}
							onChange={(event) => setVolumeName(event.target.value)}
							disabled={busy}
						>
							{candidates.map((volume) => (
								<option key={volume.id} value={volume.name}>
									{volume.name} · {formatBytes(volume.sizeBytes)}
								</option>
							))}
						</select>
					</div>
				) : (
					<p {...stylex.props(styles.hint)}>
						No unmounted volume in this environment. Create one with the Volume
						button in the top bar.
					</p>
				)}

				{targetName && (
					<form
						{...stylex.props(styles.pathRow)}
						onSubmit={(event) => {
							event.preventDefault();
							if (pathError || busy) return;
							void save({ volumeName: targetName, mountPath: path.trim() });
						}}
					>
						<div {...stylex.props(styles.pathField)}>
							<label {...stylex.props(fieldStyles.label)} htmlFor={pathId}>
								Mount path
							</label>
							<TextInput
								id={pathId}
								value={path}
								onChange={(event) => setPath(event.target.value)}
								placeholder={DEFAULT_VOLUME_MOUNT_PATH}
								autoComplete="off"
								spellCheck={false}
								disabled={busy}
							/>
						</div>
						{(!mountedName || pathChanged) && (
							<Button
								type="submit"
								variant="primary"
								disabled={
									busy || Boolean(pathError) || (!mountedName && replicas > 1)
								}
							>
								{busy && (
									<Loader2 size={13} {...stylex.props(styles.spinner)} />
								)}
								{mountedName ? "Move mount" : "Attach"}
							</Button>
						)}
					</form>
				)}
				{targetName && pathError && (
					<p {...stylex.props(styles.fieldError)}>{pathError}</p>
				)}
				{!mountedName && replicas > 1 && candidates.length > 0 && (
					<p {...stylex.props(styles.hint)}>
						Scale to one replica before attaching a volume.
					</p>
				)}
			</div>
			{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}
		</PanelSection>
	);
}

const styles = stylex.create({
	body: { display: "flex", flexDirection: "column", gap: space.md },
	unapplied: {
		borderStyle: "solid",
		borderWidth: "1px",
		padding: space.md,
	},
	mounted: { display: "flex", alignItems: "center", gap: space.sm },
	icon: { flexShrink: "0", color: colors.muted },
	volumeName: {
		fontFamily: fonts.mono,
		fontSize: "13px",
		color: colors.ink,
	},
	usage: { fontFamily: fonts.mono, fontSize: "11px", color: colors.dim },
	spacer: { flex: "1" },
	select: { cursor: "pointer", fontFamily: fonts.sans },
	pathRow: { display: "flex", alignItems: "flex-end", gap: space.md },
	pathField: { flex: "1", minWidth: "0rem" },
	hint: {
		margin: "0rem",
		fontSize: "12px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	fieldError: {
		margin: "0rem",
		fontSize: "12px",
		lineHeight: "1.5",
		color: colors.failed,
	},
	spinner: { animation: `${spin} 1s linear infinite` },
});
