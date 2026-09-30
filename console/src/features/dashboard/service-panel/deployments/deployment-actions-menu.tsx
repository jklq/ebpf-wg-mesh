import * as stylex from "@stylexjs/stylex";
import { MoreVertical } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { actionLabel } from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import { shortId } from "#/features/dashboard/shared/service-utils";
import type {
	DashboardAllocationStatus,
	DashboardDeploymentAction,
} from "#/lib/dashboard/core/types.server";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	root: { position: "relative", flexShrink: "0" },
	trigger: {
		display: "inline-flex",
		width: "2rem",
		height: "2rem",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: {
			default: "transparent",
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(255,255,255,0.05)",
			},
			":focus-visible": "rgba(255,255,255,0.05)",
			':is([aria-expanded="true"])': "rgba(255,255,255,0.05)",
		},
		padding: "0rem",
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
			":focus-visible": colors.ink,
			':is([aria-expanded="true"])': colors.ink,
		},
	},
	menu: {
		position: "absolute",
		top: "calc(100% + 6px)",
		right: "0rem",
		zIndex: "30",
		display: "flex",
		minWidth: "184px",
		flexDirection: "column",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		padding: "0.375rem",
		boxShadow: "0 14px 36px rgba(0,0,0,0.45)",
	},
	restartTargetLabel: {
		marginBottom: space.xs,
		display: "flex",
		flexDirection: "column",
		gap: space.xs,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: "rgba(80,76,71,0.45)",
		paddingInline: "0.625rem",
		paddingTop: "7px",
		paddingBottom: space.sm,
		fontSize: "10px",
		letterSpacing: "0.05em",
		color: colors.dim,
		textTransform: "uppercase",
	},
	restartTargetInput: {
		height: "27px",
		width: "100%",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		fontFamily: fonts.mono,
		fontSize: "10px",
		color: colors.ink,
	},
	menuItem: {
		minHeight: "2.25rem",
		width: "100%",
		cursor: { default: "pointer", ":disabled": "default" },
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: "transparent",
		paddingInline: "0.625rem",
		textAlign: "left",
		fontSize: "13px",
		fontWeight: "500",
		opacity: { default: null, ":disabled": "50%" },
	},
	removeItem: {
		color: {
			default: colors.failed,
			":hover": { default: null, "@media (hover: hover)": "#e08989" },
			":focus-visible": "#e08989",
		},
		backgroundColor: {
			default: null,
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(208,85,85,0.1)",
			},
			":focus-visible": "rgba(208,85,85,0.1)",
		},
	},
	defaultItem: {
		color: {
			default: colors.muted,
			":hover": { default: null, "@media (hover: hover)": colors.ink },
			":focus-visible": colors.ink,
		},
		backgroundColor: {
			default: null,
			":hover": {
				default: null,
				"@media (hover: hover)": "rgba(255,255,255,0.055)",
			},
			":focus-visible": "rgba(255,255,255,0.055)",
		},
	},
});
export function DeploymentActionsMenu({
	actions,
	pendingAction,
	allocations = [],
	restartAllocationId = "",
	styles: overrides,
	onRestartAllocationChange,
	onAction,
}: {
	actions: Array<DashboardDeploymentAction>;
	pendingAction?: DashboardDeploymentAction;
	allocations?: Array<DashboardAllocationStatus>;
	restartAllocationId?: string;
	styles?: stylex.StyleXStyles;
	onRestartAllocationChange?: (allocationId: string) => void;
	onAction: (
		action: DashboardDeploymentAction,
		allocationId?: string,
	) => void | Promise<void>;
}) {
	const [open, setOpen] = useState(false);
	const menuRef = useRef<HTMLDivElement>(null);
	const orderedActions = [
		...actions.filter((action) => action !== "DEPLOYMENT_ACTION_REMOVE"),
		...actions.filter((action) => action === "DEPLOYMENT_ACTION_REMOVE"),
	];

	useEffect(() => {
		if (!open) return;
		const onPointerDown = (event: MouseEvent) => {
			if (!menuRef.current?.contains(event.target as Node)) setOpen(false);
		};
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key === "Escape") setOpen(false);
		};
		document.addEventListener("mousedown", onPointerDown);
		document.addEventListener("keydown", onKeyDown);
		return () => {
			document.removeEventListener("mousedown", onPointerDown);
			document.removeEventListener("keydown", onKeyDown);
		};
	}, [open]);

	return (
		<div {...stylex.props([styles.root, overrides])} ref={menuRef}>
			<button
				type="button"
				{...stylex.props(styles.trigger)}
				aria-label="Deployment actions"
				aria-expanded={open}
				onClick={() => setOpen((current) => !current)}
			>
				<MoreVertical size={20} />
			</button>
			{open && (
				<div {...stylex.props(styles.menu)} role="menu">
					{actions.includes("DEPLOYMENT_ACTION_RESTART") &&
						allocations.length > 1 &&
						onRestartAllocationChange && (
							<label {...stylex.props(styles.restartTargetLabel)}>
								<span>Restart target</span>
								<select
									{...stylex.props(styles.restartTargetInput)}
									value={restartAllocationId}
									onChange={(event) =>
										onRestartAllocationChange(event.target.value)
									}
									disabled={Boolean(pendingAction)}
								>
									<option value="">All replicas</option>
									{allocations.map((entry) => (
										<option key={entry.allocationId} value={entry.allocationId}>
											{shortId(entry.allocationId)} on {shortId(entry.agentId)}
										</option>
									))}
								</select>
							</label>
						)}
					{orderedActions.map((action) => (
						<button
							key={action}
							type="button"
							role="menuitem"
							{...stylex.props([
								styles.menuItem,
								action === "DEPLOYMENT_ACTION_REMOVE"
									? styles.removeItem
									: styles.defaultItem,
							])}
							onClick={() => {
								setOpen(false);
								void onAction(
									action,
									action === "DEPLOYMENT_ACTION_RESTART"
										? restartAllocationId || undefined
										: undefined,
								);
							}}
							disabled={Boolean(pendingAction)}
						>
							{pendingAction === action ? "Working…" : actionLabel(action)}
						</button>
					))}
				</div>
			)}
		</div>
	);
}
