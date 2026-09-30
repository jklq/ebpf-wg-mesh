import * as stylex from "@stylexjs/stylex";
import { badgeStylesFor } from "#/components/ui/badge";
import { StatusDot } from "#/components/ui/status-dot";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { Loader2, Server, Terminal } from "lucide-react";
import type { MouseEvent as ReactMouseEvent } from "react";
import { NODE_H, NODE_W } from "#/features/dashboard/canvas/layout";
import {
	serviceHealth,
	shortSha,
} from "#/features/dashboard/shared/service-utils";
import { usePulseDelay } from "#/hooks/use-pulse-delay";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

export function ServiceNode({
	service,
	pos,
	selected,
	pending = false,
	onMouseDown,
	onSelect,
}: {
	service: DashboardServiceRecord;
	pos: { x: number; y: number };
	selected: boolean;
	pending?: boolean;
	onMouseDown: (event: ReactMouseEvent<HTMLElement>) => void;
	onSelect: () => void;
}) {
	const health = serviceHealth(service);
	const source = service.spec?.source?.sourceSpec;
	const repo = source?.repositorySelector ?? "";
	const repoShort = repo.split("/").pop() ?? repo;
	const unappliedCount =
		service.unappliedChangeCount ?? (service.pendingChanges ? 1 : 0);
	const pulseDelay = usePulseDelay(health === "building");

	return (
		<button
			type="button"
			data-service-node=""
			{...stylex.props([
				styles.node,
				styles.dimensions(NODE_W, NODE_H),
				styles.position(pos.x, pos.y),
				selected ? styles.selectedNode : styles.hoverableNode,
			])}
			aria-pressed={selected}
			onMouseDown={onMouseDown}
			onClick={onSelect}
		>
			<div {...stylex.props(styles.header)}>
				<StatusDot
					health={health}
					delay={health === "building" ? pulseDelay : undefined}
				/>
				<span {...stylex.props(styles.serviceName)}>{service.name}</span>
			</div>

			<div {...stylex.props(styles.content)}>
				{pending && (
					<div {...stylex.props(styles.creationStatus)}>
						<Loader2 size={10} {...stylex.props(styles.spinner)} />
						Creating service…
					</div>
				)}
				{repoShort && (
					<div {...stylex.props(styles.repository)}>
						<Server size={10} {...stylex.props(styles.metadataIcon)} />
						{repoShort}
					</div>
				)}

				{source?.trackedRef && (
					<div {...stylex.props(styles.trackedRef)}>
						<Terminal size={10} {...stylex.props(styles.metadataIcon)} />
						{source.trackedRef}
					</div>
				)}

				<div {...stylex.props(styles.footer)}>
					<div {...stylex.props(styles.pendingChanges)}>
						{unappliedCount > 0 && (
							<span {...stylex.props(badgeStylesFor("edited"))}>
								{unappliedCount} {unappliedCount === 1 ? "change" : "changes"}
							</span>
						)}
					</div>

					{service.latestBuild?.commitSha && (
						<span {...stylex.props(styles.commitSha)}>
							{shortSha(service.latestBuild.commitSha)}
						</span>
					)}
				</div>
			</div>
		</button>
	);
}

const styles = stylex.create({
	dimensions: (width: number, height: number) => ({ width, height }),
	position: (x: number, y: number) => ({ left: x, top: y }),
	node: {
		position: "absolute",

		cursor: "pointer",
		overflow: "hidden",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		padding: "0rem",
		textAlign: "left",
		color: "inherit",
		WebkitUserSelect: "none",
		userSelect: "none",
		transitionProperty: "border-color,box-shadow",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
	},
	selectedNode: {
		borderColor: colors.accent,
		boxShadow: `3px 3px 0 rgba(0,0,0,0.6), 0 0 0 1px ${colors.accent}`,
	},
	hoverableNode: {
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
		},
		boxShadow: {
			default: null,
			":hover": {
				default: null,
				"@media (hover: hover)": "3px 3px 0 rgba(0,0,0,0.5)",
			},
		},
	},
	header: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: "0.875rem",
		paddingTop: "0.625rem",
		paddingBottom: space.sm,
	},
	serviceName: {
		flex: "1",
		overflow: "hidden",
		fontFamily: fonts.condensed,
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		fontWeight: "700",
		letterSpacing: "0.03em",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.ink,
	},
	content: {
		display: "flex",
		flexDirection: "column",
		gap: "5px",
		paddingInline: "0.875rem",
		paddingTop: space.sm,
		paddingBottom: "0.625rem",
	},
	creationStatus: {
		display: "flex",
		alignItems: "center",
		gap: "5px",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.building,
	},
	spinner: { flexShrink: "0", animation: `${spin} 1s linear infinite` },
	repository: {
		display: "flex",
		alignItems: "center",
		gap: "5px",
		overflow: "hidden",
		fontFamily: fonts.mono,
		fontSize: "11px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.muted,
	},
	metadataIcon: { flexShrink: "0" },
	trackedRef: {
		display: "flex",
		alignItems: "center",
		gap: "5px",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.dim,
	},
	footer: {
		marginTop: "0.125rem",
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: "0.375rem",
	},
	pendingChanges: { display: "flex", alignItems: "center", gap: "5px" },
	commitSha: { fontFamily: fonts.mono, fontSize: "10px", color: colors.dim },
});
