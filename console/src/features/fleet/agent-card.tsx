import * as stylex from "@stylexjs/stylex";
import { AlertTriangle } from "lucide-react";
import { Button } from "#/components/ui/button";
import type {
	DashboardAgentLifecycleState,
	DashboardFleetAgent,
} from "#/lib/dashboard/core/types.server";
import { safeInteger } from "#/lib/platform-json";
import { formatRelativeTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	metricLabel: {
		fontSize: "11px",
		letterSpacing: "0.025em",
		color: colors.dim,
		textTransform: "uppercase",
	},
	card: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		padding: space.lg,
	},
	header: {
		display: "flex",
		alignItems: "flex-start",
		justifyContent: "space-between",
		gap: space.lg,
		flexDirection: { default: null, "@media (width < 800px)": "column" },
	},
	agentName: {
		marginInline: "9px",
		marginBlock: "0rem",
		display: "inline",
		fontSize: "1.125rem",
		lineHeight: "calc(1.75 / 1.125)",
	},
	agentId: { fontFamily: fonts.mono, fontSize: "11px", color: colors.dim },
	actions: {
		display: "flex",
		flexWrap: "wrap",
		justifyContent: {
			default: "flex-end",
			"@media (width < 800px)": "flex-start",
		},
		gap: "7px",
	},
	metrics: {
		marginTop: "18px",
		display: "grid",
		gridTemplateColumns: {
			default: "repeat(4, minmax(0, 1fr))",
			"@media (width < 800px)": "repeat(2, minmax(0, 1fr))",
		},
		gap: "0.875rem",
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: colors.line,
		paddingTop: "0.875rem",
	},
	versionWarning: {
		marginTop: space.md,
		marginBottom: "0rem",
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.building,
		backgroundColor: colors.buildingDim,
		padding: "0.625rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.building,
	},
	maintenanceMessage: {
		marginTop: space.md,
		marginBottom: "0rem",
		color: colors.muted,
	},
	metric: {
		display: "flex",
		minWidth: "0rem",
		flexDirection: "column",
		gap: space.xs,
	},
	metricValue: {
		fontFamily: fonts.mono,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		fontWeight: "400",
		overflowWrap: "anywhere",
	},
	lifecycleBadge: {
		display: "inline-block",
		borderStyle: "solid",
		borderWidth: "1px",
		paddingInline: "0.375rem",
		paddingBlock: "0.125rem",
		fontFamily: fonts.mono,
		fontSize: "10px",
		textTransform: "uppercase",
	},
	activeLifecycle: {
		borderColor: colors.healthy,
		backgroundColor: colors.healthyDim,
		color: colors.healthy,
	},
	transitionalLifecycle: {
		borderColor: colors.building,
		backgroundColor: colors.buildingDim,
		color: colors.building,
	},
	unavailableLifecycle: {
		borderColor: colors.failed,
		backgroundColor: colors.failedDim,
		color: colors.failed,
	},
	unknownLifecycle: { borderColor: colors.lineBright, color: colors.muted },
});
export function AgentCard({
	agent,
	busy,
	onEdit,
	onLifecycle,
}: {
	agent: DashboardFleetAgent;
	busy: boolean;
	onEdit: () => void;
	onLifecycle: (state: DashboardAgentLifecycleState) => void;
}) {
	const canRetire =
		agent.allocationCount === 0 &&
		[
			"AGENT_LIFECYCLE_STATE_ENROLLING",
			"AGENT_LIFECYCLE_STATE_CORDONED",
			"AGENT_LIFECYCLE_STATE_DRAINING",
			"AGENT_LIFECYCLE_STATE_UNAVAILABLE",
		].includes(agent.lifecycleState);
	return (
		<article {...stylex.props(styles.card)}>
			<div {...stylex.props(styles.header)}>
				<div>
					<span {...stylex.props(fleetStateStyles(agent.lifecycleState))}>
						{fleetStateLabel(agent.lifecycleState)}
					</span>
					<h2 {...stylex.props(styles.agentName)}>{agent.name}</h2>
					<code {...stylex.props(styles.agentId)}>{agent.id}</code>
				</div>
				<div {...stylex.props(styles.actions)}>
					<Button
						type="button"
						variant="ghost"
						disabled={
							busy || agent.lifecycleState === "AGENT_LIFECYCLE_STATE_RETIRED"
						}
						onClick={onEdit}
					>
						Edit
					</Button>
					{agent.lifecycleState === "AGENT_LIFECYCLE_STATE_ACTIVE" && (
						<Button
							type="button"
							variant="ghost"
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_CORDONED")}
						>
							Cordon
						</Button>
					)}
					{(agent.lifecycleState === "AGENT_LIFECYCLE_STATE_ACTIVE" ||
						agent.lifecycleState === "AGENT_LIFECYCLE_STATE_CORDONED") && (
						<Button
							type="button"
							variant="ghost"
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_DRAINING")}
						>
							Drain
						</Button>
					)}
					{(agent.lifecycleState === "AGENT_LIFECYCLE_STATE_CORDONED" ||
						agent.lifecycleState === "AGENT_LIFECYCLE_STATE_DRAINING") && (
						<Button
							type="button"
							variant="ghost"
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_ACTIVE")}
						>
							Return active
						</Button>
					)}
					{canRetire && (
						<Button
							type="button"
							variant="dangerOutline"
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_RETIRED")}
						>
							Retire
						</Button>
					)}
				</div>
			</div>
			<div {...stylex.props(styles.metrics)}>
				<Metric
					label="Failure domain"
					value={`${agent.hostType === "intermittent" ? "Intermittent · " : ""}${agent.region}${agent.zone ? ` / ${agent.zone}` : ""} / ${agent.failureDomain}`}
				/>
				<Metric label="Allocations" value={String(agent.allocationCount)} />
				<Metric
					label="CPU"
					value={`${formatCPU(agent.allocatedCpuMillis)} / ${formatCPU(agent.schedulableCpuMillis)}`}
				/>
				<Metric
					label="Memory"
					value={`${formatMemory(agent.allocatedMemoryMebibytes)} / ${formatMemory(agent.schedulableMemoryMebibytes)}`}
				/>
				<Metric
					label="Reserved"
					value={`${formatCPU(agent.reservedCpuMillis)} · ${formatMemory(agent.reservedMemoryMebibytes)}`}
				/>
				<Metric
					label="Heartbeat"
					value={formatRelativeTime(agent.lastSeenAt, Date.now(), "Never")}
				/>
				<Metric
					label="Version"
					value={agent.softwareVersion || "Not reported"}
				/>
				<Metric
					label="Capabilities"
					value={agent.runtimeCapabilities.join(", ") || "Not reported"}
				/>
			</div>
			{agent.versionSkewWarning && (
				<div {...stylex.props(styles.versionWarning)}>
					<AlertTriangle size={13} /> {agent.versionSkewWarning}
				</div>
			)}
			{agent.maintenanceMessage && (
				<p {...stylex.props(styles.maintenanceMessage)}>
					{agent.maintenanceMessage}
				</p>
			)}
		</article>
	);
}
export function Metric({ label, value }: { label: string; value: string }) {
	return (
		<div {...stylex.props(styles.metric)}>
			<span {...stylex.props(styles.metricLabel)}>{label}</span>
			<strong {...stylex.props(styles.metricValue)}>{value}</strong>
		</div>
	);
}
export function fleetStateStyles(
	state: DashboardAgentLifecycleState,
): stylex.StyleXStyles {
	const base = styles.lifecycleBadge;
	switch (state) {
		case "AGENT_LIFECYCLE_STATE_ACTIVE":
			return [base, styles.activeLifecycle];
		case "AGENT_LIFECYCLE_STATE_DRAINING":
		case "AGENT_LIFECYCLE_STATE_CORDONED":
		case "AGENT_LIFECYCLE_STATE_ENROLLING":
			return [base, styles.transitionalLifecycle];
		case "AGENT_LIFECYCLE_STATE_UNAVAILABLE":
		case "AGENT_LIFECYCLE_STATE_RETIRED":
			return [base, styles.unavailableLifecycle];
		default:
			return [base, styles.unknownLifecycle];
	}
}
export function fleetStateLabel(state: DashboardAgentLifecycleState): string {
	return state.replace("AGENT_LIFECYCLE_STATE_", "").toLowerCase();
}
export function formatCPU(raw: string): string {
	const millis = safeInteger(raw);
	return millis >= 1000
		? `${(millis / 1000).toFixed(1)} cores`
		: `${millis} mCPU`;
}
export function formatMemory(raw: string): string {
	const mebibytes = safeInteger(raw);
	return mebibytes >= 1024
		? `${(mebibytes / 1024).toFixed(1)} GiB`
		: `${mebibytes} MiB`;
}
