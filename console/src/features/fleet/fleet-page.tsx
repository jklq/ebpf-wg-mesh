import * as stylex from "@stylexjs/stylex";
import { AlertTriangle, Plus, RefreshCw, Server } from "lucide-react";
import { useState } from "react";
import { PageShell } from "#/components/layout/page-shell";
import { Button } from "#/components/ui/button";
import { textStyles } from "#/components/ui/text";
import {
	AgentCard,
	formatCPU,
	formatMemory,
} from "#/features/fleet/agent-card";
import {
	EnrollmentSecret,
	FleetAgentDialog,
} from "#/features/fleet/fleet-agent-dialog";
import {
	doSetFleetAgentLifecycle,
	loadFleet,
} from "#/features/fleet/server/functions";
import type {
	DashboardAgentEnrollment,
	DashboardAgentLifecycleState,
	DashboardFleet,
	DashboardFleetAgent,
} from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function FleetPage({ initialFleet }: { initialFleet: DashboardFleet }) {
	const [fleet, setFleet] = useState<DashboardFleet>(initialFleet);
	const [editing, setEditing] = useState<DashboardFleetAgent | "new">();
	const [enrollment, setEnrollment] = useState<DashboardAgentEnrollment>();
	const [busyAgentId, setBusyAgentId] = useState<string>();
	const [error, setError] = useState<string>();

	const refresh = async () => {
		setError(undefined);
		try {
			setFleet(await loadFleet());
		} catch (cause) {
			setError(formatError(cause));
		}
	};

	const changeLifecycle = async (
		agent: DashboardFleetAgent,
		lifecycleState: DashboardAgentLifecycleState,
	) => {
		if (lifecycleState === "AGENT_LIFECYCLE_STATE_DRAINING") {
			const accepted = window.confirm(
				`Drain ${agent.name}? Stateless allocations will move through failover. Volume-backed allocations remain fenced until stateful handoff is available.`,
			);
			if (!accepted) return;
		}
		if (lifecycleState === "AGENT_LIFECYCLE_STATE_RETIRED") {
			const accepted = window.confirm(
				`Retire ${agent.name}? This permanently revokes its credentials and removes its mesh identity.`,
			);
			if (!accepted) return;
		}
		setBusyAgentId(agent.id);
		setError(undefined);
		try {
			await doSetFleetAgentLifecycle({
				data: { agentId: agent.id, lifecycleState },
			});
			await refresh();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusyAgentId(undefined);
		}
	};

	return (
		<PageShell
			title="Agent fleet"
			icon={<Server size={15} />}
			width="wide"
			actions={
				<>
					<Button type="button" variant="ghost" onClick={() => void refresh()}>
						<RefreshCw size={13} /> Refresh
					</Button>
					<Button
						type="button"
						variant="primary"
						onClick={() => setEditing("new")}
					>
						<Plus size={13} /> Enroll node
					</Button>
				</>
			}
		>
			<div>
				<p {...stylex.props(textStyles.eyebrow)}>Platform operations</p>
				<h1 {...stylex.props(styles.title)}>Compute capacity</h1>
				<p {...stylex.props(styles.description)}>
					Operator-owned topology, reservations, health, and maintenance.
				</p>
			</div>

			<div {...stylex.props(styles.capacityGrid)}>
				<CapacityCard
					label="Schedulable nodes"
					value={`${fleet.capacity?.schedulableNodeCount ?? 0} / ${fleet.capacity?.nodeCount ?? 0}`}
				/>
				<CapacityCard
					label="CPU headroom"
					value={formatCPU(fleet.capacity?.headroomCpuMillis ?? "0")}
					detail={`${formatCPU(fleet.capacity?.allocatedCpuMillis ?? "0")} allocated`}
				/>
				<CapacityCard
					label="Memory headroom"
					value={formatMemory(fleet.capacity?.headroomMemoryMebibytes ?? "0")}
					detail={`${formatMemory(fleet.capacity?.allocatedMemoryMebibytes ?? "0")} allocated`}
				/>
			</div>

			{fleet.versionWarning && (
				<div {...stylex.props(styles.versionWarning)}>
					<AlertTriangle size={15} /> {fleet.versionWarning}
				</div>
			)}
			{error && <div {...stylex.props(styles.errorMessage)}>{error}</div>}

			<div {...stylex.props(styles.agentList)}>
				{fleet.agents.map((agent) => (
					<AgentCard
						key={agent.id}
						agent={agent}
						busy={busyAgentId === agent.id}
						onEdit={() => setEditing(agent)}
						onLifecycle={(state) => void changeLifecycle(agent, state)}
					/>
				))}
				{fleet.agents.length === 0 && (
					<div {...stylex.props(styles.emptyState)}>
						No fleet nodes are enrolled.
					</div>
				)}
			</div>

			{editing && (
				<FleetAgentDialog
					agent={editing === "new" ? undefined : editing}
					onClose={() => setEditing(undefined)}
					onSaved={async (nextEnrollment) => {
						setEditing(undefined);
						if (nextEnrollment) setEnrollment(nextEnrollment);
						await refresh();
					}}
				/>
			)}
			{enrollment && (
				<EnrollmentSecret
					enrollment={enrollment}
					onClose={() => setEnrollment(undefined)}
				/>
			)}
		</PageShell>
	);
}

function CapacityCard({
	label,
	value,
	detail,
}: {
	label: string;
	value: string;
	detail?: string;
}) {
	return (
		<div {...stylex.props(styles.capacityCard)}>
			<span {...stylex.props(styles.capacityLabel)}>{label}</span>
			<strong {...stylex.props(styles.capacityValue)}>{value}</strong>
			{detail && (
				<small {...stylex.props(styles.capacityLabel)}>{detail}</small>
			)}
		</div>
	);
}

const styles = stylex.create({
	title: {
		marginTop: "0.125rem",
		marginBottom: "5px",
		fontFamily: fonts.display,
		fontSize: "32px",
		fontWeight: "500",
	},
	description: { margin: "0rem", color: colors.muted },
	capacityGrid: {
		marginTop: "26px",
		marginBottom: "18px",
		display: "grid",
		gridTemplateColumns: {
			default: "repeat(3, minmax(0, 1fr))",
			"@media (width < 800px)": "repeat(1, minmax(0, 1fr))",
		},
		gap: space.md,
	},
	versionWarning: {
		marginBlock: space.md,
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.building,
		backgroundColor: colors.buildingDim,
		padding: "0.625rem",
		color: colors.building,
	},
	errorMessage: {
		marginBlock: space.md,
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.failed,
		backgroundColor: colors.failedDim,
		padding: "0.625rem",
		color: colors.failed,
	},
	agentList: {
		marginTop: "1.25rem",
		display: "flex",
		flexDirection: "column",
		gap: space.md,
	},
	emptyState: {
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: colors.line,
		padding: "50px",
		textAlign: "center",
		color: colors.muted,
	},
	capacityCard: {
		display: "flex",
		flexDirection: "column",
		gap: space.xs,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		padding: space.lg,
	},
	capacityLabel: {
		fontSize: "11px",
		letterSpacing: "0.025em",
		color: colors.dim,
		textTransform: "uppercase",
	},
	capacityValue: {
		fontFamily: fonts.mono,
		fontSize: "22px",
		fontWeight: "500",
	},
});
