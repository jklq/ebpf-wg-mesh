import { AlertTriangle, ArrowLeft, Plus, RefreshCw, Server } from "lucide-react";
import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";

import type {
	DashboardAgentEnrollment,
	DashboardAgentLifecycleState,
	DashboardFleet,
	DashboardFleetAgent,
	FleetAgentInput,
} from "#/lib/dashboard/core/types.server";

import {
	doCreateFleetAgent,
	doSetFleetAgentLifecycle,
	doUpdateFleetAgent,
	loadFleet,
} from "./-fleet/server-fns";
import { formatError } from "./-dashboard/service-utils";

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
		if (lifecycleState === "draining") {
			const accepted = window.confirm(
				`Drain ${agent.name}? Stateless allocations will move through failover. Volume-backed allocations remain fenced until stateful handoff is available.`,
			);
			if (!accepted) return;
		}
		if (lifecycleState === "retired") {
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
		<main className="fleet-page">
			<header className="fleet-topbar">
				<a href="/" className="btn-ghost fleet-back">
					<ArrowLeft size={13} /> Services
				</a>
				<div className="fleet-title">
					<Server size={15} />
					<strong>Agent fleet</strong>
				</div>
				<div className="fleet-topbar-spacer" />
				<button type="button" className="btn-ghost" onClick={() => void refresh()}>
					<RefreshCw size={13} /> Refresh
				</button>
				<button type="button" className="btn-primary" onClick={() => setEditing("new")}>
					<Plus size={13} /> Enroll node
				</button>
			</header>

			<section className="fleet-content">
				<div className="fleet-heading">
					<div>
						<p className="panel-eyebrow">Platform operations</p>
						<h1>Compute capacity</h1>
						<p>Operator-owned topology, reservations, health, and maintenance.</p>
					</div>
				</div>

				<div className="fleet-capacity-grid">
					<CapacityCard label="Schedulable nodes" value={`${fleet.capacity.schedulableNodeCount} / ${fleet.capacity.nodeCount}`} />
					<CapacityCard label="CPU headroom" value={formatCPU(fleet.capacity.headroomCpuMillis)} detail={`${formatCPU(fleet.capacity.allocatedCpuMillis)} allocated`} />
					<CapacityCard label="Memory headroom" value={formatMemory(fleet.capacity.headroomMemoryMebibytes)} detail={`${formatMemory(fleet.capacity.allocatedMemoryMebibytes)} allocated`} />
				</div>

				{fleet.versionWarning && (
					<div className="fleet-warning"><AlertTriangle size={15} /> {fleet.versionWarning}</div>
				)}
				{error && <div className="fleet-error">{error}</div>}

				<div className="fleet-agent-list">
					{fleet.agents.map((agent) => (
						<AgentCard
							key={agent.id}
							agent={agent}
							busy={busyAgentId === agent.id}
							onEdit={() => setEditing(agent)}
							onLifecycle={(state) => void changeLifecycle(agent, state)}
						/>
					))}
					{fleet.agents.length === 0 && <div className="fleet-empty">No fleet nodes are enrolled.</div>}
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
				<EnrollmentSecret enrollment={enrollment} onClose={() => setEnrollment(undefined)} />
			)}
		</main>
	);
}

function CapacityCard({ label, value, detail }: { label: string; value: string; detail?: string }) {
	return <div className="fleet-capacity-card"><span>{label}</span><strong>{value}</strong>{detail && <small>{detail}</small>}</div>;
}

function AgentCard({ agent, busy, onEdit, onLifecycle }: { agent: DashboardFleetAgent; busy: boolean; onEdit: () => void; onLifecycle: (state: DashboardAgentLifecycleState) => void }) {
	const canRetire = agent.allocationCount === 0 && ["enrolling", "cordoned", "draining", "unavailable"].includes(agent.lifecycleState);
	return (
		<article className="fleet-agent-card">
			<div className="fleet-agent-head">
				<div><span className={`fleet-state state-${agent.lifecycleState}`}>{agent.lifecycleState}</span><h2>{agent.name}</h2><code>{agent.id}</code></div>
				<div className="fleet-actions">
					<button type="button" className="btn-ghost" disabled={busy || agent.lifecycleState === "retired"} onClick={onEdit}>Edit</button>
					{agent.lifecycleState === "active" && <button type="button" className="btn-ghost" disabled={busy} onClick={() => onLifecycle("cordoned")}>Cordon</button>}
					{(agent.lifecycleState === "active" || agent.lifecycleState === "cordoned") && <button type="button" className="btn-ghost" disabled={busy} onClick={() => onLifecycle("draining")}>Drain</button>}
					{(agent.lifecycleState === "cordoned" || agent.lifecycleState === "draining") && <button type="button" className="btn-ghost" disabled={busy} onClick={() => onLifecycle("active")}>Return active</button>}
					{canRetire && <button type="button" className="btn-danger-outline" disabled={busy} onClick={() => onLifecycle("retired")}>Retire</button>}
				</div>
			</div>
			<div className="fleet-agent-metrics">
				<Metric label="Failure domain" value={`${agent.region}${agent.zone ? ` / ${agent.zone}` : ""} / ${agent.failureDomain}`} />
				<Metric label="Allocations" value={String(agent.allocationCount)} />
				<Metric label="CPU" value={`${formatCPU(agent.allocatedCpuMillis)} / ${formatCPU(agent.schedulableCpuMillis)}`} />
				<Metric label="Memory" value={`${formatMemory(agent.allocatedMemoryMebibytes)} / ${formatMemory(agent.schedulableMemoryMebibytes)}`} />
				<Metric label="Reserved" value={`${formatCPU(agent.reservedCpuMillis)} · ${formatMemory(agent.reservedMemoryMebibytes)}`} />
				<Metric label="Heartbeat" value={agent.lastSeenAt ? relativeTime(agent.lastSeenAt) : "Never"} />
				<Metric label="Version" value={agent.softwareVersion || "Not reported"} />
				<Metric label="Capabilities" value={agent.runtimeCapabilities.join(", ") || "Not reported"} />
			</div>
			{agent.versionSkewWarning && <div className="fleet-inline-warning"><AlertTriangle size={13} /> {agent.versionSkewWarning}</div>}
			{agent.maintenanceMessage && <p className="fleet-maintenance">{agent.maintenanceMessage}</p>}
		</article>
	);
}

function Metric({ label, value }: { label: string; value: string }) {
	return <div className="fleet-metric"><span>{label}</span><strong>{value}</strong></div>;
}

function FleetAgentDialog({ agent, onClose, onSaved }: { agent?: DashboardFleetAgent; onClose: () => void; onSaved: (enrollment?: DashboardAgentEnrollment) => Promise<void> }) {
	const [draft, setDraft] = useState<FleetAgentInput>({ agentId: agent?.id ?? "", name: agent?.name ?? "", region: agent?.region ?? "", zone: agent?.zone ?? "", failureDomain: agent?.failureDomain ?? "", reservedCpuMillis: agent?.reservedCpuMillis ?? 0, reservedMemoryMebibytes: agent?.reservedMemoryMebibytes ?? 0 });
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const save = async () => {
		setSaving(true); setError(undefined);
		try {
			if (agent) {
				await doUpdateFleetAgent({ data: draft });
				await onSaved();
			} else {
				await onSaved(await doCreateFleetAgent({ data: draft }));
			}
		} catch (cause) { setError(formatError(cause)); setSaving(false); }
	};
	return (
		<div className="modal-overlay" role="dialog" aria-modal="true" aria-label={agent ? "Edit fleet node" : "Enroll fleet node"}>
			<div className="modal-card fleet-dialog">
				<h2>{agent ? "Edit fleet node" : "Enroll fleet node"}</h2>
				<p>Topology and reservations are operator policy. The agent only reports observed host capacity and capabilities.</p>
				<div className="field-grid">
					<FleetField label="Agent ID" value={draft.agentId} disabled={Boolean(agent)} onChange={(agentId) => setDraft({ ...draft, agentId })} />
					<FleetField label="Name" value={draft.name} onChange={(name) => setDraft({ ...draft, name })} />
					<FleetField label="Region" value={draft.region} onChange={(region) => setDraft({ ...draft, region })} />
					<FleetField label="Zone (optional)" value={draft.zone} onChange={(zone) => setDraft({ ...draft, zone })} />
					<FleetField label="Failure domain" value={draft.failureDomain} onChange={(failureDomain) => setDraft({ ...draft, failureDomain })} />
					<FleetField label="Reserved CPU (mCPU)" type="number" value={String(draft.reservedCpuMillis)} onChange={(value) => setDraft({ ...draft, reservedCpuMillis: Number(value) })} />
					<FleetField label="Reserved memory (MiB)" type="number" value={String(draft.reservedMemoryMebibytes)} onChange={(value) => setDraft({ ...draft, reservedMemoryMebibytes: Number(value) })} />
				</div>
				{error && <p className="error-msg">{error}</p>}
				<div className="fleet-dialog-actions"><button type="button" className="btn-ghost" disabled={saving} onClick={onClose}>Cancel</button><button type="button" className="btn-primary" disabled={saving} onClick={() => void save()}>{saving ? "Saving…" : agent ? "Save policy" : "Create enrollment"}</button></div>
			</div>
		</div>
	);
}

function FleetField({ label, value, onChange, disabled, type = "text" }: { label: string; value: string; onChange: (value: string) => void; disabled?: boolean; type?: "text" | "number" }) {
	return <label><span className="field-label">{label}</span><input className="field-input" type={type} min={type === "number" ? 0 : undefined} value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)} /></label>;
}

function EnrollmentSecret({ enrollment, onClose }: { enrollment: DashboardAgentEnrollment; onClose: () => void }) {
	return <div className="modal-overlay" role="dialog" aria-modal="true" aria-label="Agent bootstrap token"><div className="modal-card fleet-dialog"><h2>Enrollment created</h2><p>Copy this single-use secret now. It will not be shown again.</p><code className="fleet-secret">{enrollment.bootstrapToken}</code><div className="fleet-dialog-actions"><button type="button" className="btn-primary" onClick={onClose}>I stored the token</button></div></div></div>;
}

function formatCPU(millis: number): string { return millis >= 1000 ? `${(millis / 1000).toFixed(1)} cores` : `${millis} mCPU`; }
function formatMemory(mebibytes: number): string { return mebibytes >= 1024 ? `${(mebibytes / 1024).toFixed(1)} GiB` : `${mebibytes} MiB`; }
function relativeTime(value: Date): string { const seconds = Math.max(0, Math.round((Date.now() - new Date(value).getTime()) / 1000)); if (seconds < 60) return `${seconds}s ago`; if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`; return `${Math.floor(seconds / 3600)}h ago`; }
