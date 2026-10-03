import * as stylex from "@stylexjs/stylex";
import {
	ArrowUpRight,
	Box,
	HardDrive,
	Link2,
	Loader2,
	Trash2,
	Unlink,
	X,
} from "lucide-react";
import { useState } from "react";
import { DeleteResourceDialog } from "#/components/delete-resource-dialog";
import { Badge } from "#/components/ui/badge";
import { Button } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";
import { sizeGibError } from "#/features/dashboard/volumes/volume-dialogs";
import {
	DEFAULT_VOLUME_MOUNT_PATH,
	volumeOwner,
	volumeStateLabel,
	volumeTone,
	volumeUsage,
} from "#/features/dashboard/volumes/volume-model";
import { bytesToGib, formatBytes, gibToBytes } from "#/lib/bytes";
import type {
	DashboardServiceRecord,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import {
	doDeleteResource,
	doGrowVolume,
	doUpdateService,
	fetchDeletionPreview,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { cleanDate, formatRelativeTime } from "#/lib/time";
import {
	colors,
	fonts,
	motion,
	shape,
	sizes,
	space,
} from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
const rise = stylex.keyframes({
	from: { opacity: "0", transform: "translateY(6px)" },
});

const GROW_STEPS_GIB = [5, 10, 25];

/** Side panel for one volume: a capacity gauge first, everything else terse. */
export function VolumePanel({
	volume,
	services,
	onClose,
	onMount,
	onSelectService,
	onVolumeUpdated,
	onVolumeDeleted,
	onServiceUpdated,
}: {
	volume: DashboardVolume;
	services: Array<DashboardServiceRecord>;
	onClose: () => void;
	onMount: () => void;
	onSelectService: (serviceId: string) => void;
	onVolumeUpdated: (volume: DashboardVolume) => void;
	onVolumeDeleted: (volumeId: string) => void;
	onServiceUpdated: (service: DashboardServiceRecord) => void;
}) {
	const owner = volumeOwner(volume, services);
	const tone = volumeTone(volume);
	const createdAt = cleanDate(volume.createdAt);
	const [targetGib, setTargetGib] = useState<number>();

	return (
		<>
			<header {...stylex.props(styles.header)}>
				<span {...stylex.props(styles.tile)}>
					<HardDrive size={15} />
				</span>
				<strong {...stylex.props(styles.name)}>{volume.name}</strong>
				{volume.staged ? (
					<Badge tone="edited">New</Badge>
				) : (
					<Badge tone={tone}>{volumeStateLabel(volume)}</Badge>
				)}
				<span {...stylex.props(styles.spacer)} />
				<Button
					type="button"
					variant="panelIcon"
					onClick={onClose}
					title="Close volume panel"
				>
					<X size={14} />
				</Button>
			</header>

			<div {...stylex.props(styles.body)}>
				<div {...stylex.props(styles.reveal, styles.delay(0))}>
					<Gauge volume={volume} targetGib={targetGib} />
				</div>

				<dl {...stylex.props(styles.facts, styles.reveal, styles.delay(60))}>
					<Fact label="Node">
						{volume.agentName || volume.agentId || (
							<span {...stylex.props(styles.pending)}>on first deploy</span>
						)}
					</Fact>
					<Fact label="Created">
						{createdAt ? formatRelativeTime(createdAt, Date.now()) : "—"}
					</Fact>
					<Fact label="Backups">
						<span {...stylex.props(styles.pending)}>none</span>
					</Fact>
				</dl>

				<div {...stylex.props(styles.reveal, styles.delay(120))}>
					<MountCard
						volume={volume}
						owner={owner}
						onMount={onMount}
						onSelectService={onSelectService}
						onServiceUpdated={onServiceUpdated}
					/>
				</div>

				<div {...stylex.props(styles.reveal, styles.delay(180))}>
					<GrowControl
						key={volume.sizeBytes}
						volume={volume}
						targetGib={targetGib}
						onTargetChange={setTargetGib}
						onVolumeUpdated={(updated) => {
							setTargetGib(undefined);
							onVolumeUpdated(updated);
						}}
					/>
				</div>

				<span {...stylex.props(styles.spacer)} />

				<DeleteAction
					volume={volume}
					owner={owner}
					onDeleted={() => onVolumeDeleted(volume.id)}
				/>
			</div>
		</>
	);
}

function Gauge({
	volume,
	targetGib,
}: {
	volume: DashboardVolume;
	targetGib: number | undefined;
}) {
	const { used, size, ratio } = volumeUsage(volume);
	const target = targetGib ? gibToBytes(targetGib) : size;
	const scale = Math.max(target, size, 1);
	const reported = Boolean(volume.observedAt);
	const tone = volumeTone(volume);
	const full = volume.state === "VOLUME_STATE_FULL";
	const percent = Math.round(ratio * 100);

	return (
		<section aria-label="Capacity" {...stylex.props(styles.gauge)}>
			<div {...stylex.props(styles.readout)}>
				<span {...stylex.props(styles.usedValue)}>
					{reported ? formatBytes(used) : "—"}
				</span>
				<span {...stylex.props(styles.ofValue)}>/ {formatBytes(size)}</span>
				<span {...stylex.props(styles.spacer)} />
				<span
					{...stylex.props(
						styles.percent,
						full && styles.percentFull,
						!reported && styles.percentIdle,
					)}
				>
					{reported ? `${percent}%` : volume.staged ? "staged" : "idle"}
				</span>
			</div>
			<div {...stylex.props(styles.track)}>
				<span
					{...stylex.props(
						styles.used,
						full ? styles.usedFull : ratio >= 0.8 && styles.usedHigh,
						styles.width(`${(used / scale) * 100}%`),
					)}
				/>
				{target > size && (
					<span
						{...stylex.props(
							styles.added,
							styles.from(`${(size / scale) * 100}%`),
						)}
					/>
				)}
				<span aria-hidden="true" {...stylex.props(styles.ticks)} />
			</div>
			{target > size ? (
				<p {...stylex.props(styles.caption, styles.captionAccent)}>
					+{formatBytes(target - size)} → {formatBytes(target)}
				</p>
			) : volume.stateMessage && tone !== "healthy" ? (
				<p {...stylex.props(styles.caption, styles.captionProblem)}>
					{volume.stateMessage}
				</p>
			) : !reported ? (
				<p {...stylex.props(styles.caption)}>
					{volume.staged
						? "Created on next deploy"
						: "Waiting for first report"}
				</p>
			) : null}
		</section>
	);
}

function Fact({
	label,
	children,
}: {
	label: string;
	children: React.ReactNode;
}) {
	return (
		<div {...stylex.props(styles.fact)}>
			<dt {...stylex.props(styles.factLabel)}>{label}</dt>
			<dd {...stylex.props(styles.factValue)}>{children}</dd>
		</div>
	);
}

function MountCard({
	volume,
	owner,
	onMount,
	onSelectService,
	onServiceUpdated,
}: {
	volume: DashboardVolume;
	owner: DashboardServiceRecord | undefined;
	onMount: () => void;
	onSelectService: (serviceId: string) => void;
	onServiceUpdated: (service: DashboardServiceRecord) => void;
}) {
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	if (!owner) {
		return (
			<button
				type="button"
				{...stylex.props(styles.mountEmpty)}
				onClick={onMount}
			>
				<Link2 size={14} />
				Mount {volume.name} to a service
			</button>
		);
	}
	const detach = async () => {
		setBusy(true);
		setError(undefined);
		try {
			onServiceUpdated(
				await doUpdateService({
					data: { serviceId: owner.id, volumeMount: null },
				}),
			);
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};
	return (
		<>
			<div {...stylex.props(styles.mountCard)}>
				<span {...stylex.props(styles.mountIcon)}>
					<Box size={14} />
				</span>
				<button
					type="button"
					{...stylex.props(styles.mountTarget)}
					onClick={() => onSelectService(owner.id)}
				>
					<span {...stylex.props(styles.mountService)}>
						{owner.name}
						<ArrowUpRight size={12} {...stylex.props(styles.mountArrow)} />
					</span>
					<span {...stylex.props(styles.mountPath)}>
						{owner.spec?.runtime?.volume?.mountPath ||
							DEFAULT_VOLUME_MOUNT_PATH}
					</span>
				</button>
				<Button
					type="button"
					variant="panelIcon"
					title={`Detach from ${owner.name}`}
					onClick={() => void detach()}
					disabled={busy}
				>
					{busy ? (
						<Loader2 size={13} {...stylex.props(styles.spinner)} />
					) : (
						<Unlink size={13} />
					)}
				</Button>
			</div>
			{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}
		</>
	);
}

function GrowControl({
	volume,
	targetGib,
	onTargetChange,
	onVolumeUpdated,
}: {
	volume: DashboardVolume;
	targetGib: number | undefined;
	onTargetChange: (gib: number | undefined) => void;
	onVolumeUpdated: (volume: DashboardVolume) => void;
}) {
	const currentGib = bytesToGib(volume.sizeBytes || "0");
	const [custom, setCustom] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const customError = custom ? sizeGibError(custom, currentGib) : undefined;
	const ready =
		targetGib !== undefined && targetGib > currentGib && !customError;

	const grow = async () => {
		if (!ready || busy || targetGib === undefined) return;
		setBusy(true);
		setError(undefined);
		try {
			onVolumeUpdated(
				await doGrowVolume({
					data: { volumeId: volume.id, sizeBytes: gibToBytes(targetGib) },
				}),
			);
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<section aria-label="Grow volume" {...stylex.props(styles.grow)}>
			<span {...stylex.props(styles.growLabel)}>Grow</span>
			<div {...stylex.props(styles.steps)}>
				{GROW_STEPS_GIB.map((step) => {
					const next = currentGib + step;
					const active = targetGib === next && !custom;
					return (
						<button
							key={step}
							type="button"
							aria-pressed={active}
							disabled={busy}
							{...stylex.props(styles.step, active && styles.stepActive)}
							onClick={() => {
								setCustom("");
								onTargetChange(active ? undefined : next);
							}}
						>
							+{step}
						</button>
					);
				})}
				<label
					{...stylex.props(styles.custom, custom !== "" && styles.stepActive)}
				>
					<input
						aria-label="Size (GiB)"
						value={custom}
						placeholder={String(currentGib)}
						inputMode="decimal"
						autoComplete="off"
						disabled={busy}
						{...stylex.props(styles.customInput)}
						onChange={(event) => {
							const value = event.target.value;
							setCustom(value);
							const parsed = Number(value);
							onTargetChange(
								value && Number.isFinite(parsed) ? parsed : undefined,
							);
						}}
					/>
					<span {...stylex.props(styles.customUnit)}>GiB</span>
				</label>
			</div>
			{customError && (
				<p {...stylex.props(styles.caption, styles.captionProblem)}>
					{customError}
				</p>
			)}
			{ready && (
				<Button
					type="button"
					variant="primary"
					styles={[styles.growConfirm]}
					disabled={busy}
					onClick={() => void grow()}
				>
					{busy && <Loader2 size={13} {...stylex.props(styles.spinner)} />}
					Grow to {targetGib} GiB
				</Button>
			)}
			{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}
		</section>
	);
}

function DeleteAction({
	volume,
	owner,
	onDeleted,
}: {
	volume: DashboardVolume;
	owner: DashboardServiceRecord | undefined;
	onDeleted: () => void;
}) {
	const [confirming, setConfirming] = useState(false);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const remove = async (confirmationName?: string) => {
		setBusy(true);
		setError(undefined);
		try {
			await doDeleteResource({
				data: { kind: "volume", id: volume.id, confirmationName },
			});
			setConfirming(false);
			onDeleted();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};
	return (
		<footer {...stylex.props(styles.footer)}>
			<button
				type="button"
				{...stylex.props(styles.deleteButton)}
				disabled={Boolean(owner) || busy}
				title={owner ? `Detach from ${owner.name} first` : undefined}
				onClick={() => {
					// A staged volume has no data yet: discarding it is not destructive.
					if (volume.staged) void remove();
					else setConfirming(true);
				}}
			>
				<Trash2 size={12} />
				{volume.staged ? "Discard volume" : "Delete volume"}
			</button>
			{error && !confirming && (
				<p {...stylex.props(noticeStyles.error)}>{error}</p>
			)}
			{confirming && (
				<DeleteResourceDialog
					title="Delete Volume"
					name={volume.name}
					recovery="permanent"
					requireName
					previewServicesLabel="Mounted by"
					loadPreview={() =>
						fetchDeletionPreview({ data: { kind: "volume", id: volume.id } })
					}
					busy={busy}
					error={error}
					description={
						<>
							<strong>{volume.name}</strong> and its data are destroyed after
							the deletion grace period.
						</>
					}
					onCancel={() => {
						if (!busy) setConfirming(false);
					}}
					onConfirm={(confirmationName) => void remove(confirmationName)}
				/>
			)}
		</footer>
	);
}

const styles = stylex.create({
	reveal: {
		animationName: rise,
		animationDuration: "320ms",
		animationTimingFunction: motion.ease,
		animationFillMode: "both",
	},
	delay: (ms: number) => ({ animationDelay: `${ms}ms` }),
	width: (width: string) => ({ width }),
	from: (left: string) => ({ left }),
	spacer: { flex: "1" },
	header: {
		display: "flex",
		height: sizes.header,
		flexShrink: "0",
		alignItems: "center",
		gap: space.sm,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "rgba(24,23,21,0.92)",
		paddingInline: space.lg,
	},
	tile: {
		display: "inline-flex",
		width: "28px",
		height: "28px",
		flexShrink: "0",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(226,138,36,0.35)",
		borderRadius: shape.card,
		backgroundColor: colors.accentDim,
		color: colors.accent,
	},
	name: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontFamily: fonts.display,
		fontSize: "21px",
		fontWeight: "500",
		letterSpacing: "-0.03em",
		color: colors.ink,
	},
	body: {
		position: "relative",
		display: "flex",
		minHeight: "0rem",
		flex: "1",
		flexDirection: "column",
		gap: space.xl,
		overflowY: "auto",
		padding: "22px 18px 18px",
	},

	gauge: { display: "flex", flexDirection: "column", gap: "0.625rem" },
	readout: { display: "flex", alignItems: "baseline", gap: space.sm },
	usedValue: {
		fontFamily: fonts.mono,
		fontSize: "34px",
		fontWeight: "500",
		lineHeight: "1",
		letterSpacing: "-0.04em",
		color: colors.ink,
	},
	ofValue: { fontFamily: fonts.mono, fontSize: "13px", color: colors.dim },
	percent: {
		fontFamily: fonts.condensed,
		fontSize: "15px",
		fontWeight: "700",
		letterSpacing: "0.06em",
		textTransform: "uppercase",
		color: colors.accent,
	},
	percentFull: { color: colors.failed },
	percentIdle: { color: colors.dim },
	track: {
		position: "relative",
		height: "18px",
		overflow: "hidden",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.card,
		backgroundColor: colors.canvas,
	},
	used: {
		position: "absolute",
		insetBlock: "0rem",
		left: "0rem",
		backgroundImage: `linear-gradient(90deg, rgba(226,138,36,0.55), ${colors.accent})`,
		boxShadow: "0 0 18px rgba(226,138,36,0.35)",
		transitionProperty: "width",
		transitionDuration: "420ms",
		transitionTimingFunction: motion.ease,
	},
	usedHigh: {
		backgroundImage: `linear-gradient(90deg, rgba(212,154,42,0.55), ${colors.building})`,
	},
	usedFull: {
		backgroundImage: `linear-gradient(90deg, rgba(208,85,85,0.55), ${colors.failed})`,
		boxShadow: "0 0 18px rgba(208,85,85,0.35)",
	},
	added: {
		position: "absolute",
		insetBlock: "0rem",
		right: "0rem",
		borderLeftStyle: "solid",
		borderLeftWidth: "1px",
		borderLeftColor: colors.accent,
		backgroundImage:
			"repeating-linear-gradient(135deg, rgba(226,138,36,0.28) 0 4px, transparent 4px 8px)",
	},
	ticks: {
		pointerEvents: "none",
		position: "absolute",
		inset: "0",
		backgroundImage:
			"repeating-linear-gradient(90deg, transparent 0 calc(10% - 1px), rgba(240,232,220,0.08) calc(10% - 1px) 10%)",
	},
	caption: {
		margin: "0rem",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.dim,
	},
	captionAccent: { color: colors.accent },
	captionProblem: { color: colors.failed },

	facts: {
		display: "grid",
		gridTemplateColumns: "repeat(3, minmax(0, 1fr))",
		margin: "0rem",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.card,
		backgroundColor: "color-mix(in oklab, #fff 2%, transparent)",
	},
	fact: {
		display: "flex",
		minWidth: "0rem",
		flexDirection: "column",
		gap: "3px",
		borderLeftStyle: "solid",
		borderLeftWidth: { default: "1px", ":first-child": "0px" },
		borderLeftColor: colors.line,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
	},
	factLabel: {
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		letterSpacing: "0.1em",
		textTransform: "uppercase",
		color: colors.dim,
	},
	factValue: {
		margin: "0rem",
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "12px",
		color: colors.label,
	},
	pending: { color: colors.dim },

	mountCard: {
		display: "flex",
		alignItems: "center",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.card,
		backgroundColor: colors.surfaceRaised,
		padding: "0.625rem 0.5rem 0.625rem 0.75rem",
	},
	mountIcon: {
		display: "inline-flex",
		width: "26px",
		height: "26px",
		flexShrink: "0",
		alignItems: "center",
		justifyContent: "center",
		borderRadius: shape.card,
		backgroundColor: colors.surfaceHover,
		color: colors.muted,
	},
	mountTarget: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		flexDirection: "column",
		gap: "1px",
		cursor: "pointer",
		borderWidth: "0px",
		backgroundColor: "transparent",
		padding: "0rem",
		textAlign: "left",
		color: {
			default: colors.ink,
			":hover": { default: null, "@media (hover: hover)": colors.accent },
		},
	},
	mountService: {
		display: "inline-flex",
		alignItems: "center",
		gap: space.xs,
		fontSize: "14px",
		fontWeight: "600",
	},
	mountArrow: { opacity: "0.6" },
	mountPath: { fontFamily: fonts.mono, fontSize: "11px", color: colors.dim },
	mountEmpty: {
		display: "flex",
		width: "100%",
		alignItems: "center",
		justifyContent: "center",
		gap: space.sm,
		cursor: "pointer",
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.accent },
		},
		borderRadius: shape.card,
		backgroundColor: "transparent",
		paddingBlock: "0.875rem",
		fontFamily: fonts.condensed,
		fontSize: "13px",
		fontWeight: "700",
		letterSpacing: "0.05em",
		textTransform: "uppercase",
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
		},
		transitionProperty: "border-color, color",
		transitionDuration: motion.fast,
	},

	grow: { display: "flex", flexDirection: "column", gap: space.sm },
	growLabel: {
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		letterSpacing: "0.1em",
		textTransform: "uppercase",
		color: colors.dim,
	},
	steps: { display: "flex", gap: "6px" },
	step: {
		flex: "1",
		cursor: "pointer",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
		},
		borderRadius: shape.card,
		backgroundColor: colors.surface,
		paddingBlock: "0.5rem",
		fontFamily: fonts.mono,
		fontSize: "12px",
		color: colors.label,
		transitionProperty: "border-color, background-color, color",
		transitionDuration: motion.fast,
	},
	stepActive: {
		borderColor: colors.accent,
		backgroundColor: colors.accentDim,
		color: colors.ink,
	},
	custom: {
		display: "flex",
		flex: "1.4",
		alignItems: "center",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		borderRadius: shape.card,
		backgroundColor: colors.surface,
		paddingInline: space.sm,
	},
	customInput: {
		minWidth: "0rem",
		flex: "1",
		borderWidth: "0px",
		outline: "none",
		backgroundColor: "transparent",
		paddingBlock: "0.5rem",
		fontFamily: fonts.mono,
		fontSize: "12px",
		color: colors.ink,
		"::placeholder": { color: colors.dim },
	},
	customUnit: { fontFamily: fonts.mono, fontSize: "11px", color: colors.dim },
	growConfirm: { width: "100%", justifyContent: "center" },

	footer: {
		display: "flex",
		flexDirection: "column",
		alignItems: "flex-start",
		gap: space.sm,
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderTopColor: colors.line,
		paddingTop: space.md,
	},
	deleteButton: {
		display: "inline-flex",
		alignItems: "center",
		gap: "6px",
		cursor: { default: "pointer", ":disabled": "not-allowed" },
		borderWidth: "0px",
		backgroundColor: "transparent",
		padding: "0rem",
		fontSize: "12px",
		color: {
			default: colors.dim,
			":hover": { default: null, "@media (hover: hover)": colors.failed },
			":disabled": colors.offline,
		},
		transitionProperty: "color",
		transitionDuration: motion.fast,
	},
	spinner: { animation: `${spin} 1s linear infinite` },
});
