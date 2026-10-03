import * as stylex from "@stylexjs/stylex";
import { HardDrive, Loader2, X } from "lucide-react";
import { type ReactNode, useEffect, useId, useRef, useState } from "react";
import { Button } from "#/components/ui/button";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import {
	DEFAULT_VOLUME_MOUNT_PATH,
	DEFAULT_VOLUME_SIZE_GIB,
	MAX_VOLUME_SIZE_GIB,
	MIN_VOLUME_SIZE_GIB,
	mountPathError,
	serviceVolume,
	suggestVolumeName,
	volumeNameError,
} from "#/features/dashboard/volumes/volume-model";
import { gibToBytes } from "#/lib/bytes";
import type {
	DashboardServiceRecord,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import {
	doCreateVolume,
	doUpdateService,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

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

export function sizeGibError(raw: string, minimum = MIN_VOLUME_SIZE_GIB) {
	const value = Number(raw.trim());
	if (!Number.isFinite(value) || raw.trim() === "")
		return "Enter a size in GiB";
	if (value < minimum) return `Size must be at least ${minimum} GiB`;
	if (value > MAX_VOLUME_SIZE_GIB)
		return `Size must be at most ${MAX_VOLUME_SIZE_GIB} GiB`;
	if (!Number.isInteger(value * 1024)) return "Use at most three decimals";
	return undefined;
}

function DialogFrame({
	label,
	title,
	busy,
	onClose,
	onSubmit,
	children,
}: {
	label: string;
	title: string;
	busy: boolean;
	onClose: () => void;
	onSubmit: () => void;
	children: ReactNode;
}) {
	return (
		<Dialog
			label={label}
			onClose={() => {
				if (!busy) onClose();
			}}
		>
			<form
				{...stylex.props([dialogStyles.card, styles.form])}
				onSubmit={(event) => {
					event.preventDefault();
					onSubmit();
				}}
			>
				<div {...stylex.props(styles.header)}>
					<h2 {...stylex.props(styles.title)}>
						<HardDrive size={17} {...stylex.props(styles.titleIcon)} />
						{title}
					</h2>
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
				{children}
			</form>
		</Dialog>
	);
}

function MountFields({
	services,
	serviceId,
	mountPath,
	allowNone,
	disabled,
	onServiceChange,
	onMountPathChange,
}: {
	services: Array<DashboardServiceRecord>;
	serviceId: string;
	mountPath: string;
	allowNone: boolean;
	disabled: boolean;
	onServiceChange: (id: string) => void;
	onMountPathChange: (path: string) => void;
}) {
	const serviceFieldId = useId();
	const pathFieldId = useId();
	const pathError = serviceId ? mountPathError(mountPath) : undefined;
	return (
		<>
			<div>
				<label {...stylex.props(fieldStyles.label)} htmlFor={serviceFieldId}>
					Mount to service
				</label>
				<select
					id={serviceFieldId}
					{...stylex.props([fieldStyles.input, styles.select])}
					value={serviceId}
					onChange={(event) => onServiceChange(event.target.value)}
					disabled={disabled}
				>
					{allowNone && <option value="">Don't mount yet</option>}
					{services.map((service) => (
						<option key={service.id} value={service.id}>
							{service.name}
						</option>
					))}
				</select>
				{services.length === 0 && (
					<p {...stylex.props(styles.hint)}>
						Every service already mounts a volume or runs several replicas.
					</p>
				)}
			</div>
			{serviceId && (
				<div>
					<label {...stylex.props(fieldStyles.label)} htmlFor={pathFieldId}>
						Mount path
					</label>
					<TextInput
						id={pathFieldId}
						value={mountPath}
						onChange={(event) => onMountPathChange(event.target.value)}
						placeholder={DEFAULT_VOLUME_MOUNT_PATH}
						autoComplete="off"
						spellCheck={false}
						disabled={disabled}
					/>
					<p {...stylex.props(pathError ? styles.fieldError : styles.hint)}>
						{pathError ?? "Applies with the next deploy."}
					</p>
				</div>
			)}
		</>
	);
}

export function CreateVolumeDialog({
	environmentId,
	services,
	volumes,
	onClose,
	onCreated,
	onServiceUpdated,
}: {
	environmentId: string;
	services: Array<DashboardServiceRecord>;
	volumes: Array<DashboardVolume>;
	onClose: () => void;
	onCreated: (volume: DashboardVolume) => void;
	onServiceUpdated: (service: DashboardServiceRecord) => void;
}) {
	const [name, setName] = useState(() =>
		suggestVolumeName(volumes.map((volume) => volume.name)),
	);
	const [sizeGib, setSizeGib] = useState(String(DEFAULT_VOLUME_SIZE_GIB));
	const candidates = mountableServices(services, volumes);
	const [serviceId, setServiceId] = useState("");
	const [mountPath, setMountPath] = useState(DEFAULT_VOLUME_MOUNT_PATH);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const nameRef = useRef<HTMLInputElement>(null);
	const nameFieldId = useId();
	const sizeFieldId = useId();

	useEffect(() => {
		nameRef.current?.select();
	}, []);

	const nameError = volumeNameError(name);
	const sizeError = sizeGibError(sizeGib);
	const invalid =
		Boolean(nameError || sizeError) ||
		Boolean(serviceId && mountPathError(mountPath));

	const submit = async () => {
		if (busy || invalid) return;
		setBusy(true);
		setError(undefined);
		let created: DashboardVolume;
		try {
			created = await doCreateVolume({
				data: {
					environmentId,
					name: name.trim(),
					sizeBytes: gibToBytes(Number(sizeGib)),
				},
			});
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
			return;
		}
		onCreated(created);
		if (serviceId) {
			try {
				onServiceUpdated(
					await doUpdateService({
						data: {
							serviceId,
							volumeMount: {
								volumeName: created.name,
								mountPath: mountPath.trim(),
							},
						},
					}),
				);
			} catch (cause) {
				setError(`Volume created, but mounting failed: ${formatError(cause)}`);
				setBusy(false);
				return;
			}
		}
		onClose();
	};

	return (
		<DialogFrame
			label="New volume"
			title="New Volume"
			busy={busy}
			onClose={onClose}
			onSubmit={() => void submit()}
		>
			<div {...stylex.props(styles.row)}>
				<div {...stylex.props(styles.grow)}>
					<label {...stylex.props(fieldStyles.label)} htmlFor={nameFieldId}>
						Name
					</label>
					<TextInput
						id={nameFieldId}
						ref={nameRef}
						value={name}
						onChange={(event) => setName(event.target.value)}
						autoComplete="off"
						spellCheck={false}
						disabled={busy}
					/>
				</div>
				<div {...stylex.props(styles.size)}>
					<label {...stylex.props(fieldStyles.label)} htmlFor={sizeFieldId}>
						Size (GiB)
					</label>
					<TextInput
						id={sizeFieldId}
						value={sizeGib}
						onChange={(event) => setSizeGib(event.target.value)}
						inputMode="decimal"
						autoComplete="off"
						disabled={busy}
					/>
				</div>
			</div>
			{(nameError || sizeError) && (
				<p {...stylex.props(styles.fieldError)}>{nameError ?? sizeError}</p>
			)}
			<MountFields
				services={candidates}
				serviceId={serviceId}
				mountPath={mountPath}
				allowNone
				disabled={busy}
				onServiceChange={setServiceId}
				onMountPathChange={setMountPath}
			/>
			{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}
			<Button
				type="submit"
				variant="primary"
				styles={[styles.submit]}
				disabled={busy || invalid}
			>
				{busy && <Loader2 size={13} {...stylex.props(styles.spinner)} />}
				{busy ? "Creating…" : "Create volume"}
			</Button>
		</DialogFrame>
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
	const candidates = mountableServices(services, volumes);
	const [serviceId, setServiceId] = useState(candidates[0]?.id ?? "");
	const [mountPath, setMountPath] = useState(DEFAULT_VOLUME_MOUNT_PATH);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const invalid = !serviceId || Boolean(mountPathError(mountPath));

	const submit = async () => {
		if (busy || invalid) return;
		setBusy(true);
		setError(undefined);
		try {
			onServiceUpdated(
				await doUpdateService({
					data: {
						serviceId,
						volumeMount: {
							volumeName: volume.name,
							mountPath: mountPath.trim(),
						},
					},
				}),
			);
			onClose();
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<DialogFrame
			label={`Mount ${volume.name}`}
			title={`Mount ${volume.name}`}
			busy={busy}
			onClose={onClose}
			onSubmit={() => void submit()}
		>
			<MountFields
				services={candidates}
				serviceId={serviceId}
				mountPath={mountPath}
				allowNone={false}
				disabled={busy}
				onServiceChange={setServiceId}
				onMountPathChange={setMountPath}
			/>
			{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}
			<Button
				type="submit"
				variant="primary"
				styles={[styles.submit]}
				disabled={busy || invalid}
			>
				{busy && <Loader2 size={13} {...stylex.props(styles.spinner)} />}
				{busy ? "Mounting…" : "Mount volume"}
			</Button>
		</DialogFrame>
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
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		margin: "0rem",
		fontFamily: fonts.condensed,
		fontSize: "22px",
		letterSpacing: "0.02em",
		color: colors.ink,
	},
	titleIcon: { color: colors.accent },
	row: { display: "flex", gap: space.md },
	grow: { flex: "1", minWidth: "0rem" },
	size: { width: "110px", flexShrink: "0" },
	select: { cursor: "pointer", fontFamily: fonts.sans },
	hint: {
		marginTop: "0.375rem",
		marginBottom: "0rem",
		fontSize: "11px",
		lineHeight: "1.5",
		color: colors.dim,
	},
	fieldError: {
		marginTop: "0.375rem",
		marginBottom: "0rem",
		fontSize: "11px",
		lineHeight: "1.5",
		color: colors.failed,
	},
	submit: { width: "100%", justifyContent: "center", paddingBlock: "0.625rem" },
	spinner: { animation: `${spin} 1s linear infinite` },
});
