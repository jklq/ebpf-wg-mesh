import * as stylex from "@stylexjs/stylex";
import { HardDrive } from "lucide-react";
import { useId, useState } from "react";
import { badgeStylesFor } from "#/components/ui/badge";
import { StatusDot } from "#/components/ui/status-dot";
import {
	NODE_H,
	NODE_W,
	VOLUME_NODE_H,
	VOLUME_TAB_H,
} from "#/features/dashboard/canvas/layout";
import {
	volumeTone,
	volumeUsage,
} from "#/features/dashboard/volumes/volume-model";
import { formatBytes } from "#/lib/bytes";
import type { DashboardVolume } from "#/lib/dashboard/core/types.server";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

function usageDetail(volume: DashboardVolume): string {
	const { used, size, ratio } = volumeUsage(volume);
	if (!volume.observedAt) {
		return `${formatBytes(size)} capacity · usage not reported yet`;
	}
	return `${formatBytes(used)} of ${formatBytes(size)} used · ${Math.round(ratio * 100)}%`;
}

/**
 * The strip hanging under a service card for the volume it mounts. Its
 * background fills from the left with the share of capacity in use.
 */
export function VolumeTab({
	volume,
	servicePos,
	selected,
	onSelect,
}: {
	volume: DashboardVolume;
	servicePos: { x: number; y: number };
	selected: boolean;
	onSelect: () => void;
}) {
	const [hovered, setHovered] = useState(false);
	const tooltipId = useId();
	const { ratio } = volumeUsage(volume);
	const tone = volumeTone(volume);
	const fillTone =
		volume.state === "VOLUME_STATE_FULL"
			? styles.fillFull
			: ratio >= 0.8
				? styles.fillHigh
				: styles.fillNormal;
	return (
		<button
			type="button"
			data-volume-node=""
			aria-label={`Volume ${volume.name}`}
			aria-describedby={hovered ? tooltipId : undefined}
			aria-pressed={selected}
			{...stylex.props([
				styles.tab,
				styles.position(servicePos.x, servicePos.y + NODE_H),
				styles.tabSize(NODE_W, VOLUME_TAB_H),
				selected ? styles.selected : styles.hoverable,
			])}
			onMouseDown={(event) => event.stopPropagation()}
			onMouseEnter={() => setHovered(true)}
			onMouseLeave={() => setHovered(false)}
			onFocus={() => setHovered(true)}
			onBlur={() => setHovered(false)}
			onClick={(event) => {
				event.stopPropagation();
				onSelect();
			}}
		>
			<span aria-hidden="true" {...stylex.props(styles.track)}>
				<span
					{...stylex.props(
						styles.fill,
						fillTone,
						styles.fillWidth(`${ratio * 100}%`),
					)}
				/>
			</span>
			<span {...stylex.props(styles.label)}>
				<HardDrive size={14} {...stylex.props(styles.icon)} />
				<span {...stylex.props(styles.name)}>{volume.name}</span>
				{volume.staged ? (
					<span {...stylex.props(badgeStylesFor("edited"))}>new</span>
				) : (
					tone !== "healthy" && <StatusDot health={tone} />
				)}
			</span>
			{hovered && (
				<span role="tooltip" id={tooltipId} {...stylex.props(styles.tooltip)}>
					<span {...stylex.props(styles.tooltipUsage)}>
						{usageDetail(volume)}
					</span>
					{volume.stateMessage && tone !== "healthy" && (
						<span {...stylex.props(styles.tooltipMeta)}>
							{volume.stateMessage}
						</span>
					)}
				</span>
			)}
		</button>
	);
}

/** A volume no service mounts yet, with a shortcut to mount it. */
export function VolumeNode({
	volume,
	pos,
	selected,
	onSelect,
	onMount,
}: {
	volume: DashboardVolume;
	pos: { x: number; y: number };
	selected: boolean;
	onSelect: () => void;
	onMount: () => void;
}) {
	return (
		<div
			data-volume-node=""
			{...stylex.props([
				styles.node,
				styles.position(pos.x, pos.y),
				styles.tabSize(NODE_W, VOLUME_NODE_H),
				selected ? styles.selected : styles.hoverable,
			])}
		>
			<button
				type="button"
				aria-label={`Volume ${volume.name}`}
				aria-pressed={selected}
				{...stylex.props(styles.nodeSelect)}
				onClick={(event) => {
					event.stopPropagation();
					onSelect();
				}}
			>
				<HardDrive size={14} {...stylex.props(styles.icon)} />
				<span {...stylex.props(styles.nodeName)}>{volume.name}</span>
				{volume.staged && (
					<span {...stylex.props(badgeStylesFor("edited"))}>new</span>
				)}
			</button>
			<button
				type="button"
				{...stylex.props(styles.mount)}
				onClick={(event) => {
					event.stopPropagation();
					onMount();
				}}
			>
				Mount
			</button>
		</div>
	);
}

const styles = stylex.create({
	position: (x: number, y: number) => ({ left: x, top: y }),
	tabSize: (width: number, height: number) => ({ width, height }),
	fillWidth: (width: string) => ({ width }),
	tab: {
		position: "absolute",
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		cursor: "pointer",
		borderStyle: "solid",
		borderWidth: "1px",
		borderTopWidth: "0px",
		borderBottomLeftRadius: shape.card,
		borderBottomRightRadius: shape.card,
		backgroundColor: colors.surface,
		paddingInline: "0.875rem",
		textAlign: "left",
		color: "inherit",
		userSelect: "none",
		transitionProperty: "border-color, background-color",
		transitionDuration: motion.fast,
	},
	track: {
		pointerEvents: "none",
		position: "absolute",
		inset: "0",
		overflow: "hidden",
		borderBottomLeftRadius: shape.card,
		borderBottomRightRadius: shape.card,
	},
	fill: {
		position: "absolute",
		insetBlock: "0rem",
		left: "0rem",
		transitionProperty: "width",
		transitionDuration: motion.normal,
	},
	fillNormal: {
		backgroundColor: "color-mix(in oklab, #fff 6%, transparent)",
		boxShadow: "inset -1px 0 0 color-mix(in oklab, #fff 10%, transparent)",
	},
	fillHigh: {
		backgroundColor: "rgba(192,133,32,0.16)",
		boxShadow: "inset -1px 0 0 rgba(192,133,32,0.4)",
	},
	fillFull: {
		backgroundColor: "rgba(184,66,66,0.2)",
	},
	tooltip: {
		pointerEvents: "none",
		position: "absolute",
		top: "calc(100% + 6px)",
		left: "0rem",
		zIndex: "5",
		display: "flex",
		flexDirection: "column",
		gap: "2px",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		borderRadius: shape.card,
		backgroundColor: colors.surfaceRaised,
		boxShadow: "3px 3px 0 rgba(0,0,0,0.5)",
		paddingInline: space.md,
		paddingBlock: space.sm,
		whiteSpace: "nowrap",
	},
	tooltipUsage: {
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.ink,
	},
	tooltipMeta: {
		fontSize: "11px",
		color: colors.dim,
	},
	node: {
		position: "absolute",
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderRadius: shape.card,
		backgroundColor: colors.surfaceRaised,
		paddingLeft: space.md,
		paddingRight: space.sm,
		userSelect: "none",
		transitionProperty: "border-color, box-shadow",
		transitionDuration: motion.fast,
	},
	hoverable: {
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
		},
	},
	selected: {
		borderColor: colors.accent,
		boxShadow: `0 0 0 1px ${colors.accent}`,
	},
	icon: { flexShrink: "0", color: colors.muted },
	label: {
		position: "relative",
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		alignItems: "center",
		gap: space.sm,
	},
	name: {
		flex: "1",
		minWidth: "0rem",
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontFamily: fonts.sans,
		fontSize: "13px",
		color: colors.label,
	},
	nodeSelect: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		alignItems: "center",
		gap: space.sm,
		height: "100%",
		cursor: "pointer",
		borderWidth: "0px",
		backgroundColor: "transparent",
		padding: "0rem",
		color: "inherit",
		textAlign: "left",
	},
	nodeName: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontFamily: fonts.condensed,
		fontSize: "0.875rem",
		fontWeight: "700",
		letterSpacing: "0.03em",
		color: colors.ink,
	},
	mount: {
		flexShrink: "0",
		cursor: "pointer",
		borderRadius: shape.control,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.accent },
		},
		backgroundColor: colors.canvas,
		paddingInline: space.md,
		paddingBlock: "5px",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.07em",
		color: colors.ink,
	},
});
