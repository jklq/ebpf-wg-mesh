import * as stylex from "@stylexjs/stylex";
import { Database, Trash2 } from "lucide-react";
import { Button } from "#/components/ui/button";
import type { DashboardVolume } from "#/lib/dashboard/core/types.server";
import { safeInteger } from "#/lib/platform-json";
import { cleanDate, formatRelativeTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	volumeRow: {
		display: "flex",
		alignItems: "center",
		gap: space.md,
		borderTopStyle: { default: "solid", ":first-child": "solid" },
		borderTopWidth: { default: "1px", ":first-child": "0px" },
		borderColor: colors.line,
		paddingInline: "0.875rem",
		paddingBlock: "0.625rem",
	},
	volumeIcon: { flexShrink: "0", color: colors.muted },
	volumeCopy: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		flexDirection: "column",
	},
	volumeName: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "13px",
		fontWeight: "500",
		color: colors.ink,
	},
	volumeMetadata: { fontSize: "11px", color: colors.dim },
	deleteVolumeButton: {
		color: {
			default: null,
			":hover": { default: null, "@media (hover: hover)": colors.failed },
		},
	},
});
export function VolumeRow({
	nowMs,
	volume,
	onDelete,
}: {
	volume: DashboardVolume;
	nowMs: number;
	onDelete: () => void;
}) {
	const createdAt = cleanDate(volume.createdAt);
	return (
		<li {...stylex.props(styles.volumeRow)}>
			<Database size={14} {...stylex.props(styles.volumeIcon)} />
			<div {...stylex.props(styles.volumeCopy)}>
				<strong {...stylex.props(styles.volumeName)}>{volume.name}</strong>
				<span {...stylex.props(styles.volumeMetadata)}>
					{formatBytes(volume.sizeBytes)}
					{createdAt && ` · created ${formatRelativeTime(createdAt, nowMs)}`}
				</span>
			</div>
			<Button
				type="button"
				variant="panelIcon"
				styles={[styles.deleteVolumeButton]}
				onClick={onDelete}
				title={`Delete volume ${volume.name}`}
				aria-label={`Delete volume ${volume.name}`}
			>
				<Trash2 size={13} />
			</Button>
		</li>
	);
}
export function formatBytes(raw: string): string {
	const bytes = safeInteger(raw);
	const units = ["B", "KiB", "MiB", "GiB", "TiB"];
	let value = bytes;
	let unit = 0;
	while (value >= 1024 && unit < units.length - 1) {
		value /= 1024;
		unit += 1;
	}
	return `${Number.isInteger(value) ? value : value.toFixed(1)} ${units[unit]}`;
}
