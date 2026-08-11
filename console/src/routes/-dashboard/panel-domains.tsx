import { Globe, Loader2, Pencil, Trash2, X } from "lucide-react";
import { useEffect, useState } from "react";

import type {
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardProject,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import {
	doCreateDomainBinding,
	doDeleteDomainBinding,
	doRequestDomainOwnershipChallenge,
	doUpdateDomainBinding,
	fetchDomainBindings,
} from "./server-fns";
import { buildServiceURL, formatError } from "./service-utils";

export function PanelDomains({
	service,
	project,
	state,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject;
	state: DashboardHomeState;
}) {
	const recommendedPort = recommendedTargetPort(service, state);
	const [bindings, setBindings] = useState<DashboardDomainBinding[]>([]);
	const [loadingBindings, setLoadingBindings] = useState(true);
	const [hostname, setHostname] = useState("");
	const [targetPort, setTargetPort] = useState("");
	const [dnsResult, setDnsResult] = useState<{
		state: string;
		instruction: string;
	} | null>(null);
	const [checking, setChecking] = useState(false);
	const [publishing, setPublishing] = useState(false);
	const [error, setError] = useState<string>();
	const [success, setSuccess] = useState<string>();

	const [editingBinding, setEditingBinding] =
		useState<DashboardDomainBinding | null>(null);
	const [editPort, setEditPort] = useState("");
	const [editSaving, setEditSaving] = useState(false);
	const [editError, setEditError] = useState<string>();

	const [deletingHostname, setDeletingHostname] = useState<string | null>(null);
	const [deleteConfirm, setDeleteConfirm] = useState<string | null>(null);

	const hostnameId = `domain-hostname-${service.id}`;
	const targetPortId = `domain-target-port-${service.id}`;

	useEffect(() => {
		setLoadingBindings(true);
		fetchDomainBindings({
			data: { projectId: project.id, serviceId: service.id },
		})
			.then(setBindings)
			.catch(() => setBindings([]))
			.finally(() => setLoadingBindings(false));
	}, [project.id, service.id]);

	useEffect(() => {
		setTargetPort("");
	}, [service.id]);

	const handleCheckDNS = async () => {
		if (!hostname.trim()) return;
		setError(undefined);
		setDnsResult(null);
		setChecking(true);
		try {
			const result = await doRequestDomainOwnershipChallenge({
				data: { projectId: project.id, hostname: hostname.trim() },
			});
			setDnsResult({
				state: "pending",
				instruction: `Create TXT record ${result.recordName} with value ${result.recordValue}. The control plane will verify it when you publish.`,
			});
		} catch (e) {
			setError(formatError(e));
		} finally {
			setChecking(false);
		}
	};

	const handlePublish = async () => {
		setError(undefined);
		setSuccess(undefined);
		setPublishing(true);
		try {
			const binding = await doCreateDomainBinding({
				data: {
					projectId: project.id,
					serviceId: service.id,
					hostname: hostname.trim(),
					targetPort: targetPort.trim() || String(recommendedPort),
				},
			});
			setBindings((prev) => [...prev, binding]);
			setHostname("");
			setDnsResult(null);
			setSuccess(`${binding.hostname} is now live.`);
		} catch (e) {
			setError(formatError(e));
		} finally {
			setPublishing(false);
		}
	};

	const openEdit = (binding: DashboardDomainBinding) => {
		setEditingBinding(binding);
		setEditPort(String(binding.targetPort));
		setEditError(undefined);
	};

	const handleEditSave = async () => {
		if (!editingBinding) return;
		setEditError(undefined);
		setEditSaving(true);
		try {
			const updated = await doUpdateDomainBinding({
				data: {
					projectId: editingBinding.projectId,
					serviceId: editingBinding.serviceId,
					hostname: editingBinding.hostname,
					targetPort: editPort,
				},
			});
			setBindings((prev) =>
				prev.map((b) => (b.hostname === updated.hostname ? updated : b)),
			);
			setEditingBinding(null);
		} catch (e) {
			setEditError(formatError(e));
		} finally {
			setEditSaving(false);
		}
	};

	const handleDelete = async (h: string) => {
		setDeletingHostname(h);
		try {
			await doDeleteDomainBinding({
				data: { projectId: project.id, hostname: h },
			});
			setBindings((prev) => prev.filter((b) => b.hostname !== h));
		} catch (e) {
			setError(formatError(e));
		} finally {
			setDeletingHostname(null);
			setDeleteConfirm(null);
		}
	};

	return (
		<div style={{ display: "flex", flexDirection: "column", gap: 20 }}>
			{editingBinding && (
				<div className="modal-overlay" onClick={() => setEditingBinding(null)}>
					<div
						className="modal-card"
						style={{ padding: 24, display: "flex", flexDirection: "column", gap: 16 }}
						onClick={(e) => e.stopPropagation()}
					>
						<div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
							<span style={{ fontSize: 14, fontWeight: 600, color: "var(--text)" }}>
								Edit domain
							</span>
							<button
								type="button"
								className="btn-ghost"
								style={{ padding: "4px 6px" }}
								onClick={() => setEditingBinding(null)}
							>
								<X size={14} />
							</button>
						</div>
						<div>
							<p style={{ fontSize: 12, color: "var(--text-muted)", margin: "0 0 12px" }}>
								<span style={{ fontFamily: "var(--font-mono)" }}>{editingBinding.hostname}</span>
							</p>
							<label className="field-label" htmlFor="edit-port">
								App port
							</label>
							<input
								id="edit-port"
								className="field-input"
								value={editPort}
								onChange={(e) => { setEditPort(e.target.value); setEditError(undefined); }}
								placeholder="8080"
								inputMode="numeric"
							/>
						</div>
						{editError && <p className="error-msg">{editError}</p>}
						<div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
							<button
								type="button"
								className="btn-secondary"
								onClick={() => setEditingBinding(null)}
							>
								Cancel
							</button>
							<button
								type="button"
								className="btn-primary"
								onClick={handleEditSave}
								disabled={editSaving || editPort.trim() === ""}
							>
								{editSaving && (
									<Loader2 size={12} style={{ animation: "spin 1s linear infinite" }} />
								)}
								Save
							</button>
						</div>
					</div>
				</div>
			)}

			{deleteConfirm && (
				<div className="modal-overlay" onClick={() => setDeleteConfirm(null)}>
					<div
						className="modal-card"
						style={{ padding: 24, display: "flex", flexDirection: "column", gap: 16 }}
						onClick={(e) => e.stopPropagation()}
					>
						<span style={{ fontSize: 14, fontWeight: 600, color: "var(--text)" }}>
							Remove domain?
						</span>
						<p style={{ fontSize: 13, color: "var(--text-muted)", margin: 0 }}>
							<span style={{ fontFamily: "var(--font-mono)" }}>{deleteConfirm}</span>
							{" "}will stop routing traffic immediately.
						</p>
						<div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
							<button
								type="button"
								className="btn-secondary"
								onClick={() => setDeleteConfirm(null)}
								disabled={deletingHostname === deleteConfirm}
							>
								Cancel
							</button>
							<button
								type="button"
								className="btn-danger"
								onClick={() => handleDelete(deleteConfirm)}
								disabled={deletingHostname === deleteConfirm}
							>
								{deletingHostname === deleteConfirm && (
									<Loader2 size={12} style={{ animation: "spin 1s linear infinite" }} />
								)}
								Remove
							</button>
						</div>
					</div>
				</div>
			)}

			<div>
				<p className="section-header">Active domains</p>
				{loadingBindings && (
					<div
						style={{
							display: "flex",
							gap: 6,
							color: "var(--text-muted)",
							fontSize: 12,
						}}
					>
						<Loader2
							size={13}
							style={{ animation: "spin 1s linear infinite" }}
						/>
						Loading…
					</div>
				)}
				{!loadingBindings && bindings.length === 0 && (
					<p style={{ fontSize: 13, color: "var(--text-muted)", margin: 0 }}>
						No domains yet.
					</p>
				)}
				{bindings.map((binding) => (
					<div key={binding.hostname} className="domain-item">
						<div
							style={{
								display: "flex",
								alignItems: "center",
								gap: 6,
								overflow: "hidden",
							}}
						>
							<Globe size={12} color="var(--healthy)" />
							<span
								style={{
									fontSize: 13,
									fontFamily: "var(--font-mono)",
									color: "var(--text)",
									overflow: "hidden",
									textOverflow: "ellipsis",
									whiteSpace: "nowrap",
								}}
							>
								{binding.hostname}
								<span style={{ color: "var(--text-muted)" }}>
									{" "}
									-&gt; :{binding.targetPort}
								</span>
							</span>
						</div>
						<div style={{ display: "flex", alignItems: "center", gap: 4, flexShrink: 0 }}>
							<a
								href={buildServiceURL(state, binding.hostname)}
								target="_blank"
								rel="noreferrer"
								className="btn-ghost"
								style={{ fontSize: 11 }}
							>
								Open ↗
							</a>
							<button
								type="button"
								className="btn-ghost"
								style={{ padding: "4px 6px" }}
								onClick={() => openEdit(binding)}
								title="Edit"
							>
								<Pencil size={12} />
							</button>
							<button
								type="button"
								className="btn-ghost"
								style={{ padding: "4px 6px", color: "var(--danger, #e05252)" }}
								onClick={() => setDeleteConfirm(binding.hostname)}
								title="Remove"
							>
								<Trash2 size={12} />
							</button>
						</div>
					</div>
				))}
			</div>

			<hr className="divider" />

			<div>
				<p className="section-header">Add domain</p>

				<div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
					<div>
						<label className="field-label" htmlFor={hostnameId}>
							Hostname
						</label>
						<input
							id={hostnameId}
							className="field-input"
							value={hostname}
							onChange={(e) => {
								setHostname(e.target.value);
								setDnsResult(null);
								setError(undefined);
							}}
							placeholder="app.example.com"
						/>
					</div>

					<div>
						<label className="field-label" htmlFor={targetPortId}>
							App port
						</label>
						<input
							id={targetPortId}
							className="field-input"
							value={targetPort}
							onChange={(e) => {
								setTargetPort(e.target.value);
								setError(undefined);
							}}
							placeholder={String(recommendedPort)}
							inputMode="numeric"
						/>
					</div>

					{state.ingressTargetHost && (
						<div
							style={{
								fontSize: 11,
								color: "var(--text-muted)",
								padding: "8px 10px",
								background: "var(--surface-raised)",
								borderRadius: 0,
								fontFamily: "var(--font-mono)",
								lineHeight: 1.5,
							}}
						>
							Set a CNAME record: {hostname || "<hostname>"} →{" "}
							{state.ingressTargetHost}
						</div>
					)}

					{dnsResult && (
						<div className="success-msg">{dnsResult.instruction}</div>
					)}

					{error && <p className="error-msg">{error}</p>}
					{success && <p className="success-msg">{success}</p>}

					<div style={{ display: "flex", gap: 8 }}>
						<button
							type="button"
							className="btn-secondary"
							onClick={handleCheckDNS}
							disabled={checking || hostname.trim() === ""}
						>
							{checking && (
								<Loader2
									size={12}
									style={{ animation: "spin 1s linear infinite" }}
								/>
							)}
							Generate TXT proof
						</button>

						{dnsResult && (
							<button
								type="button"
								className="btn-primary"
								onClick={handlePublish}
								disabled={publishing}
							>
								{publishing && (
									<Loader2
										size={12}
										style={{ animation: "spin 1s linear infinite" }}
									/>
								)}
								Publish domain
							</button>
						)}
					</div>
				</div>
			</div>
		</div>
	);
}

function recommendedTargetPort(
	service: DashboardServiceRecord,
	state: DashboardHomeState,
): number {
	const primary = service.spec?.runtime.ports.find((port) => port.primary);
	if (primary?.port) {
		return primary.port;
	}
	const healthy = state.serviceStatus?.allocation?.healthyPorts.find(
		(port) => Number.isInteger(port) && port >= 1 && port <= 65535,
	);
	return healthy ?? 8080;
}
