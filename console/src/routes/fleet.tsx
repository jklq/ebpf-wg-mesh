import { createFileRoute } from "@tanstack/react-router";
import {
	AlertTriangle,
	ArrowLeft,
	Plus,
	RefreshCw,
	Server,
} from "lucide-react";
import { useState } from "react";

import { cn } from "#/lib/cn";
import type {
	DashboardAgentEnrollment,
	DashboardAgentLifecycleState,
	DashboardFleet,
	DashboardFleetAgent,
	FleetAgentInput,
} from "#/lib/dashboard/core/types.server";
import {
	btnDangerOutline,
	btnGhost,
	btnPrimary,
	errorMsg,
	fieldInput,
	fieldLabel,
	modalCard,
	modalOverlay,
	panelEyebrow,
} from "#/lib/ui-classes";
import { formatError } from "./-dashboard/service-utils";
import {
	doCreateFleetAgent,
	doSetFleetAgentLifecycle,
	doUpdateFleetAgent,
	loadFleet,
} from "./-fleet/server-fns";

export const Route = createFileRoute("/fleet")({
	loader: () => loadFleet(),
	component: FleetRoute,
});

function FleetRoute() {
	const [fleet, setFleet] = useState<DashboardFleet>(Route.useLoaderData());
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
		<main className="min-h-dvh bg-canvas">
			<header className="sticky top-0 z-30 flex h-header items-center gap-2.5 border-b border-line bg-surface px-4">
				<a href="/" className={btnGhost}>
					<ArrowLeft size={13} /> Services
				</a>
				<div className="ml-2 flex items-center gap-[7px]">
					<Server size={15} />
					<strong>Agent fleet</strong>
				</div>
				<div className="flex-1" />
				<button
					type="button"
					className={btnGhost}
					onClick={() => void refresh()}
				>
					<RefreshCw size={13} /> Refresh
				</button>
				<button
					type="button"
					className={btnPrimary}
					onClick={() => setEditing("new")}
				>
					<Plus size={13} /> Enroll node
				</button>
			</header>

			<section className="mx-auto w-[min(1180px,calc(100%-32px))] py-9 pb-18">
				<div>
					<p className={panelEyebrow}>Platform operations</p>
					<h1 className="mt-0.5 mb-[5px] font-display text-[32px] font-medium">
						Compute capacity
					</h1>
					<p className="m-0 text-muted">
						Operator-owned topology, reservations, health, and maintenance.
					</p>
				</div>

				<div className="mt-[26px] mb-[18px] grid grid-cols-3 gap-3 max-[800px]:grid-cols-1">
					<CapacityCard
						label="Schedulable nodes"
						value={`${fleet.capacity.schedulableNodeCount} / ${fleet.capacity.nodeCount}`}
					/>
					<CapacityCard
						label="CPU headroom"
						value={formatCPU(fleet.capacity.headroomCpuMillis)}
						detail={`${formatCPU(fleet.capacity.allocatedCpuMillis)} allocated`}
					/>
					<CapacityCard
						label="Memory headroom"
						value={formatMemory(fleet.capacity.headroomMemoryMebibytes)}
						detail={`${formatMemory(fleet.capacity.allocatedMemoryMebibytes)} allocated`}
					/>
				</div>

				{fleet.versionWarning && (
					<div className="my-3 flex items-center gap-2 border border-building bg-building-dim p-2.5 text-building">
						<AlertTriangle size={15} /> {fleet.versionWarning}
					</div>
				)}
				{error && (
					<div className="my-3 flex items-center gap-2 border border-failed bg-failed-dim p-2.5 text-failed">
						{error}
					</div>
				)}

				<div className="mt-5 flex flex-col gap-3">
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
						<div className="border border-dashed border-line p-[50px] text-center text-muted">
							No fleet nodes are enrolled.
						</div>
					)}
				</div>
			</section>

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
		</main>
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
		<div className="flex flex-col gap-1 border border-line bg-surface p-4">
			<span className="text-[11px] tracking-wide text-dim uppercase">
				{label}
			</span>
			<strong className="font-mono text-[22px] font-medium">{value}</strong>
			{detail && (
				<small className="text-[11px] tracking-wide text-dim uppercase">
					{detail}
				</small>
			)}
		</div>
	);
}

function AgentCard({
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
		<article className="border border-line bg-surface p-4">
			<div className="flex items-start justify-between gap-4 max-[800px]:flex-col">
				<div>
					<span className={fleetStateClass(agent.lifecycleState)}>
						{fleetStateLabel(agent.lifecycleState)}
					</span>
					<h2 className="mx-[9px] my-0 inline text-lg">{agent.name}</h2>
					<code className="font-mono text-[11px] text-dim">{agent.id}</code>
				</div>
				<div className="flex flex-wrap justify-end gap-[7px] max-[800px]:justify-start">
					<button
						type="button"
						className={btnGhost}
						disabled={
							busy || agent.lifecycleState === "AGENT_LIFECYCLE_STATE_RETIRED"
						}
						onClick={onEdit}
					>
						Edit
					</button>
					{agent.lifecycleState === "AGENT_LIFECYCLE_STATE_ACTIVE" && (
						<button
							type="button"
							className={btnGhost}
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_CORDONED")}
						>
							Cordon
						</button>
					)}
					{(agent.lifecycleState === "AGENT_LIFECYCLE_STATE_ACTIVE" ||
						agent.lifecycleState === "AGENT_LIFECYCLE_STATE_CORDONED") && (
						<button
							type="button"
							className={btnGhost}
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_DRAINING")}
						>
							Drain
						</button>
					)}
					{(agent.lifecycleState === "AGENT_LIFECYCLE_STATE_CORDONED" ||
						agent.lifecycleState === "AGENT_LIFECYCLE_STATE_DRAINING") && (
						<button
							type="button"
							className={btnGhost}
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_ACTIVE")}
						>
							Return active
						</button>
					)}
					{canRetire && (
						<button
							type="button"
							className={btnDangerOutline}
							disabled={busy}
							onClick={() => onLifecycle("AGENT_LIFECYCLE_STATE_RETIRED")}
						>
							Retire
						</button>
					)}
				</div>
			</div>
			<div className="mt-[18px] grid grid-cols-4 gap-3.5 border-t border-line pt-3.5 max-[800px]:grid-cols-2">
				<Metric
					label="Failure domain"
					value={`${agent.region}${agent.zone ? ` / ${agent.zone}` : ""} / ${agent.failureDomain}`}
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
					value={agent.lastSeenAt ? relativeTime(agent.lastSeenAt) : "Never"}
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
				<div className="mt-3 mb-0 flex items-center gap-2 border border-building bg-building-dim p-2.5 text-xs text-building">
					<AlertTriangle size={13} /> {agent.versionSkewWarning}
				</div>
			)}
			{agent.maintenanceMessage && (
				<p className="mt-3 mb-0 text-muted">{agent.maintenanceMessage}</p>
			)}
		</article>
	);
}

function Metric({ label, value }: { label: string; value: string }) {
	return (
		<div className="flex min-w-0 flex-col gap-1">
			<span className="text-[11px] tracking-wide text-dim uppercase">
				{label}
			</span>
			<strong className="font-mono text-xs font-normal wrap-anywhere">
				{value}
			</strong>
		</div>
	);
}

function FleetAgentDialog({
	agent,
	onClose,
	onSaved,
}: {
	agent?: DashboardFleetAgent;
	onClose: () => void;
	onSaved: (enrollment?: DashboardAgentEnrollment) => Promise<void>;
}) {
	const [draft, setDraft] = useState<FleetAgentInput>({
		agentId: agent?.id ?? "",
		name: agent?.name ?? "",
		region: agent?.region ?? "",
		zone: agent?.zone ?? "",
		failureDomain: agent?.failureDomain ?? "",
		reservedCpuMillis: agent?.reservedCpuMillis ?? 0,
		reservedMemoryMebibytes: agent?.reservedMemoryMebibytes ?? 0,
	});
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const save = async () => {
		setSaving(true);
		setError(undefined);
		try {
			if (agent) {
				await doUpdateFleetAgent({ data: draft });
				await onSaved();
			} else {
				await onSaved(await doCreateFleetAgent({ data: draft }));
			}
		} catch (cause) {
			setError(formatError(cause));
			setSaving(false);
		}
	};
	return (
		<div
			className={modalOverlay}
			role="dialog"
			aria-modal="true"
			aria-label={agent ? "Edit fleet node" : "Enroll fleet node"}
		>
			<div
				className={cn(
					modalCard,
					"!max-w-[min(620px,calc(100vw-32px))] p-[22px]",
				)}
			>
				<h2 className="mt-0">
					{agent ? "Edit fleet node" : "Enroll fleet node"}
				</h2>
				<p className="text-muted">
					Topology and reservations are operator policy. The agent only reports
					observed host capacity and capabilities.
				</p>
				<div className="my-5 grid grid-cols-2 gap-3 max-sm:grid-cols-1">
					<FleetField
						label="Agent ID"
						value={draft.agentId}
						disabled={Boolean(agent)}
						onChange={(agentId) => setDraft({ ...draft, agentId })}
					/>
					<FleetField
						label="Name"
						value={draft.name}
						onChange={(name) => setDraft({ ...draft, name })}
					/>
					<FleetField
						label="Region"
						value={draft.region}
						onChange={(region) => setDraft({ ...draft, region })}
					/>
					<FleetField
						label="Zone (optional)"
						value={draft.zone}
						onChange={(zone) => setDraft({ ...draft, zone })}
					/>
					<FleetField
						label="Failure domain"
						value={draft.failureDomain}
						onChange={(failureDomain) => setDraft({ ...draft, failureDomain })}
					/>
					<FleetField
						label="Reserved CPU (mCPU)"
						type="number"
						value={String(draft.reservedCpuMillis)}
						onChange={(value) =>
							setDraft({ ...draft, reservedCpuMillis: Number(value) })
						}
					/>
					<FleetField
						label="Reserved memory (MiB)"
						type="number"
						value={String(draft.reservedMemoryMebibytes)}
						onChange={(value) =>
							setDraft({
								...draft,
								reservedMemoryMebibytes: Number(value),
							})
						}
					/>
				</div>
				{error && <p className={errorMsg}>{error}</p>}
				<div className="mt-5 flex justify-end gap-2">
					<button
						type="button"
						className={btnGhost}
						disabled={saving}
						onClick={onClose}
					>
						Cancel
					</button>
					<button
						type="button"
						className={btnPrimary}
						disabled={saving}
						onClick={() => void save()}
					>
						{saving ? "Saving…" : agent ? "Save policy" : "Create enrollment"}
					</button>
				</div>
			</div>
		</div>
	);
}

function FleetField({
	label,
	value,
	onChange,
	disabled,
	type = "text",
}: {
	label: string;
	value: string;
	onChange: (value: string) => void;
	disabled?: boolean;
	type?: "text" | "number";
}) {
	return (
		<label>
			<span className={fieldLabel}>{label}</span>
			<input
				className={fieldInput}
				type={type}
				min={type === "number" ? 0 : undefined}
				value={value}
				disabled={disabled}
				onChange={(event) => onChange(event.target.value)}
			/>
		</label>
	);
}

function EnrollmentSecret({
	enrollment,
	onClose,
}: {
	enrollment: DashboardAgentEnrollment;
	onClose: () => void;
}) {
	return (
		<div
			className={modalOverlay}
			role="dialog"
			aria-modal="true"
			aria-label="Agent bootstrap token"
		>
			<div
				className={cn(
					modalCard,
					"!max-w-[min(620px,calc(100vw-32px))] p-[22px]",
				)}
			>
				<h2 className="mt-0">Enrollment created</h2>
				<p className="text-muted">
					Copy this single-use secret now. It will not be shown again.
				</p>
				<code className="block wrap-anywhere select-all border border-accent bg-accent-dim p-3 font-mono">
					{enrollment.bootstrapToken}
				</code>
				<div className="mt-5 flex justify-end gap-2">
					<button type="button" className={btnPrimary} onClick={onClose}>
						I stored the token
					</button>
				</div>
			</div>
		</div>
	);
}

function fleetStateClass(state: DashboardAgentLifecycleState): string {
	const base =
		"inline-block border px-1.5 py-0.5 font-mono text-[10px] uppercase";
	switch (state) {
		case "AGENT_LIFECYCLE_STATE_ACTIVE":
			return cn(base, "border-healthy bg-healthy-dim text-healthy");
		case "AGENT_LIFECYCLE_STATE_DRAINING":
		case "AGENT_LIFECYCLE_STATE_CORDONED":
		case "AGENT_LIFECYCLE_STATE_ENROLLING":
			return cn(base, "border-building bg-building-dim text-building");
		case "AGENT_LIFECYCLE_STATE_UNAVAILABLE":
		case "AGENT_LIFECYCLE_STATE_RETIRED":
			return cn(base, "border-failed bg-failed-dim text-failed");
		default:
			return cn(base, "border-line-bright text-muted");
	}
}

function fleetStateLabel(state: DashboardAgentLifecycleState): string {
	return state.replace("AGENT_LIFECYCLE_STATE_", "").toLowerCase();
}

function formatCPU(millis: number): string {
	return millis >= 1000
		? `${(millis / 1000).toFixed(1)} cores`
		: `${millis} mCPU`;
}
function formatMemory(mebibytes: number): string {
	return mebibytes >= 1024
		? `${(mebibytes / 1024).toFixed(1)} GiB`
		: `${mebibytes} MiB`;
}
function relativeTime(value: Date): string {
	const seconds = Math.max(
		0,
		Math.round((Date.now() - new Date(value).getTime()) / 1000),
	);
	if (seconds < 60) return `${seconds}s ago`;
	if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
	return `${Math.floor(seconds / 3600)}h ago`;
}
