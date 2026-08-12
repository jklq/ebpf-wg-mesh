import { CircleCheck, Globe, Loader2, Pencil, Trash2, X } from "lucide-react";
import { useEffect, useState } from "react";

import type {
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import {
	doCreateDomainBinding,
	doDeleteDomainBinding,
	doGenerateDomainBinding,
	doUpdateDomainBinding,
	fetchDomainBindings,
} from "./server-fns";
import { buildServiceURL, formatError } from "./service-utils";

export function PanelDomains({
	service,
	state,
}: {
	service: DashboardServiceRecord;
	state: DashboardHomeState;
}) {
	const recommendedPort = recommendedTargetPort(service, state);
	const [bindings, setBindings] = useState<DashboardDomainBinding[]>([]);
	const [loadingBindings, setLoadingBindings] = useState(true);
	const [hostname, setHostname] = useState("");
	const [targetPort, setTargetPort] = useState("");
	const [domainFlow, setDomainFlow] = useState<"generate" | "custom" | null>(
		null,
	);
	const [generating, setGenerating] = useState(false);
	// A domain that has been requested but is not confirmed live yet. It is shown
	// in the list straight away with a spinner so the flow never blocks on a modal.
	const [pendingDomain, setPendingDomain] = useState<{
		hostname?: string;
	} | null>(null);
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
	const platformBinding = bindings.find((binding) => binding.platformGenerated);
	const internalHostname =
		service.internalHostname ?? `${service.name}.mesh.internal`;
	const internalShortName = internalHostname.replace(/\.mesh\.internal$/, "");

	useEffect(() => {
		setLoadingBindings(true);
		setTargetPort("");
		setHostname("");
		setDomainFlow(null);
		fetchDomainBindings({
			data: { serviceId: service.id },
		})
			.then(setBindings)
			.catch(() => setBindings([]))
			.finally(() => setLoadingBindings(false));
	}, [service.id]);

	const openDomainFlow = (flow: "generate" | "custom") => {
		setDomainFlow(flow);
		setTargetPort(platformBinding ? String(platformBinding.targetPort) : "");
		setError(undefined);
		setSuccess(undefined);
	};

	const handleGenerate = async ({
		keepFlowOpen,
	}: {
		keepFlowOpen: boolean;
	}) => {
		setError(undefined);
		setSuccess(undefined);
		setGenerating(true);
		if (!keepFlowOpen) {
			// Close immediately — the pending row in the list carries the progress.
			setDomainFlow(null);
			setPendingDomain({});
		}
		try {
			const binding = await doGenerateDomainBinding({
				data: {
					serviceId: service.id,
					targetPort: targetPort.trim() || String(recommendedPort),
				},
			});
			setBindings((previous) => [
				...previous.filter((item) => !item.platformGenerated),
				binding,
			]);
			if (!keepFlowOpen) {
				// Keep the spinner on the row until the platform lists the binding —
				// that is when routing for it is actually in place.
				setPendingDomain({ hostname: binding.hostname });
				await fetchDomainBindings({ data: { serviceId: service.id } })
					.then(setBindings)
					.catch(() => undefined);
				setPendingDomain(null);
			}
			setSuccess(`${binding.hostname} is ready.`);
		} catch (e) {
			setPendingDomain(null);
			setError(formatError(e));
		} finally {
			setGenerating(false);
		}
	};

	const handlePublish = async () => {
		setError(undefined);
		setSuccess(undefined);
		setPublishing(true);
		try {
			const binding = await doCreateDomainBinding({
				data: {
					serviceId: service.id,
					hostname: hostname.trim(),
					targetPort: String(platformBinding?.targetPort ?? recommendedPort),
				},
			});
			setBindings((prev) => [...prev, binding]);
			setHostname("");
			setSuccess(`${binding.hostname} is now live.`);
			setDomainFlow(null);
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
				data: { hostname: h },
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
			{domainFlow && (
				<div
					className="modal-overlay"
					role="dialog"
					aria-modal="true"
					aria-label={
						domainFlow === "generate" ? "Generate domain" : "Custom domain"
					}
					tabIndex={-1}
					onClick={(event) => {
						if (event.target === event.currentTarget) setDomainFlow(null);
					}}
					onKeyDown={(event) => {
						if (event.key === "Escape") setDomainFlow(null);
					}}
				>
					<div
						className="modal-card"
						style={{
							padding: 24,
							display: "flex",
							flexDirection: "column",
							gap: 16,
						}}
					>
						<div
							style={{
								display: "flex",
								alignItems: "center",
								justifyContent: "space-between",
							}}
						>
							<span
								style={{ fontSize: 14, fontWeight: 600, color: "var(--text)" }}
							>
								{domainFlow === "generate"
									? "Generate Domain"
									: "Custom Domain"}
							</span>
							<button
								type="button"
								className="btn-ghost"
								style={{ padding: "4px 6px" }}
								onClick={() => setDomainFlow(null)}
								aria-label="Close"
							>
								<X size={14} />
							</button>
						</div>

						{(domainFlow === "generate" || !platformBinding) && (
							<div>
								<label className="field-label" htmlFor={targetPortId}>
									App port
								</label>
								<input
									id={targetPortId}
									className="field-input"
									value={targetPort}
									onChange={(event) => {
										setTargetPort(event.target.value);
										setError(undefined);
									}}
									placeholder={String(
										platformBinding?.targetPort ?? recommendedPort,
									)}
									inputMode="numeric"
								/>
								<p
									style={{
										fontSize: 11,
										color: "var(--text-muted)",
										margin: "6px 0 0",
									}}
								>
									The port your app listens on inside the service.
								</p>
							</div>
						)}

						{domainFlow === "generate" && platformBinding && (
							<div
								className="success-msg"
								style={{ fontFamily: "var(--font-mono)" }}
							>
								{platformBinding.hostname}
							</div>
						)}

						{domainFlow === "custom" && platformBinding && (
							<>
								<div>
									<label className="field-label" htmlFor={hostnameId}>
										Custom hostname
									</label>
									<input
										id={hostnameId}
										className="field-input"
										value={hostname}
										onChange={(event) => {
											setHostname(event.target.value);
											setError(undefined);
										}}
										placeholder="app.customer.com"
									/>
								</div>
								<div
									style={{
										fontSize: 11,
										color: "var(--text-muted)",
										padding: "10px 12px",
										background: "var(--surface-raised)",
										fontFamily: "var(--font-mono)",
										lineHeight: 1.6,
									}}
								>
									Create this DNS record:
									<br />
									{hostname || "app.customer.com"} CNAME{" "}
									{platformBinding.hostname}
								</div>
							</>
						)}

						{error && <p className="error-msg">{error}</p>}
						{success && <p className="success-msg">{success}</p>}

						<div
							style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}
						>
							<button
								type="button"
								className="btn-secondary"
								onClick={() => setDomainFlow(null)}
							>
								Cancel
							</button>
							{(domainFlow === "generate" || !platformBinding) && (
								<button
									type="button"
									className="btn-primary"
									onClick={() =>
										void handleGenerate({
											keepFlowOpen: domainFlow === "custom",
										})
									}
									disabled={generating}
								>
									{generating && (
										<Loader2
											size={12}
											style={{ animation: "spin 1s linear infinite" }}
										/>
									)}
									{domainFlow === "custom"
										? "Generate & continue"
										: "Generate Domain"}
								</button>
							)}
							{domainFlow === "custom" && platformBinding && (
								<button
									type="button"
									className="btn-primary"
									onClick={handlePublish}
									disabled={publishing || hostname.trim() === ""}
								>
									{publishing && (
										<Loader2
											size={12}
											style={{ animation: "spin 1s linear infinite" }}
										/>
									)}
									Verify CNAME & add domain
								</button>
							)}
						</div>
					</div>
				</div>
			)}

			{editingBinding && (
				<div
					className="modal-overlay"
					role="dialog"
					aria-modal="true"
					aria-label="Edit domain"
					tabIndex={-1}
					onClick={(event) => {
						if (event.target === event.currentTarget) setEditingBinding(null);
					}}
					onKeyDown={(event) => {
						if (event.key === "Escape") setEditingBinding(null);
					}}
				>
					<div
						className="modal-card"
						style={{
							padding: 24,
							display: "flex",
							flexDirection: "column",
							gap: 16,
						}}
					>
						<div
							style={{
								display: "flex",
								alignItems: "center",
								justifyContent: "space-between",
							}}
						>
							<span
								style={{ fontSize: 14, fontWeight: 600, color: "var(--text)" }}
							>
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
							<p
								style={{
									fontSize: 12,
									color: "var(--text-muted)",
									margin: "0 0 12px",
								}}
							>
								<span style={{ fontFamily: "var(--font-mono)" }}>
									{editingBinding.hostname}
								</span>
							</p>
							<label className="field-label" htmlFor="edit-port">
								App port
							</label>
							<input
								id="edit-port"
								className="field-input"
								value={editPort}
								onChange={(e) => {
									setEditPort(e.target.value);
									setEditError(undefined);
								}}
								placeholder="8080"
								inputMode="numeric"
							/>
						</div>
						{editError && <p className="error-msg">{editError}</p>}
						<div
							style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}
						>
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
									<Loader2
										size={12}
										style={{ animation: "spin 1s linear infinite" }}
									/>
								)}
								Save
							</button>
						</div>
					</div>
				</div>
			)}

			{deleteConfirm && (
				<div
					className="modal-overlay"
					role="dialog"
					aria-modal="true"
					aria-label="Remove domain"
					tabIndex={-1}
					onClick={(event) => {
						if (event.target === event.currentTarget) setDeleteConfirm(null);
					}}
					onKeyDown={(event) => {
						if (event.key === "Escape") setDeleteConfirm(null);
					}}
				>
					<div
						className="modal-card"
						style={{
							padding: 24,
							display: "flex",
							flexDirection: "column",
							gap: 16,
						}}
					>
						<span
							style={{ fontSize: 14, fontWeight: 600, color: "var(--text)" }}
						>
							Remove domain?
						</span>
						<p style={{ fontSize: 13, color: "var(--text-muted)", margin: 0 }}>
							<span style={{ fontFamily: "var(--font-mono)" }}>
								{deleteConfirm}
							</span>{" "}
							will stop routing traffic immediately.
						</p>
						<div
							style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}
						>
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
									<Loader2
										size={12}
										style={{ animation: "spin 1s linear infinite" }}
									/>
								)}
								Remove
							</button>
						</div>
					</div>
				</div>
			)}

			<div>
				<p className="section-header">Private networking</p>
				<p className="domain-internal-description">
					Communicate with this service from within the current environment.
				</p>
				<div className="domain-internal-card">
					<CircleCheck size={20} aria-hidden="true" />
					<div className="domain-internal-content">
						<div className="domain-internal-hostline">
							<span>{internalHostname}</span>
							<span className="domain-internal-protocol">IPv6</span>
						</div>
						<p>
							Ready to talk privately · You can also simply call me{" "}
							<code>{internalShortName}</code>
						</p>
					</div>
				</div>
			</div>

			<hr className="divider" />

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
				{!loadingBindings && bindings.length === 0 && !pendingDomain && (
					<p style={{ fontSize: 13, color: "var(--text-muted)", margin: 0 }}>
						No domains yet.
					</p>
				)}
				{pendingDomain && !pendingDomain.hostname && (
					<div className="domain-item domain-item-pending">
						<div
							style={{
								display: "flex",
								alignItems: "center",
								gap: 6,
								overflow: "hidden",
							}}
						>
							<Loader2
								size={12}
								style={{ animation: "spin 1s linear infinite" }}
							/>
							<span
								style={{
									fontSize: 13,
									fontFamily: "var(--font-mono)",
									color: "var(--text-muted)",
								}}
							>
								Generating domain…
							</span>
						</div>
					</div>
				)}
				{bindings.map((binding) => {
					const pending = pendingDomain?.hostname === binding.hostname;
					return (
						<div
							key={binding.hostname}
							className={`domain-item ${pending ? "domain-item-pending" : ""}`}
						>
							<div
								style={{
									display: "flex",
									alignItems: "center",
									gap: 6,
									overflow: "hidden",
								}}
							>
								{pending ? (
									<Loader2
										size={12}
										style={{ animation: "spin 1s linear infinite" }}
									/>
								) : (
									<Globe size={12} color="var(--healthy)" />
								)}
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
							<div
								style={{
									display: "flex",
									alignItems: "center",
									gap: 4,
									flexShrink: 0,
								}}
							>
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
									style={{
										padding: "4px 6px",
										color: "var(--danger, #e05252)",
									}}
									onClick={() => setDeleteConfirm(binding.hostname)}
									title="Remove"
								>
									<Trash2 size={12} />
								</button>
							</div>
						</div>
					);
				})}
				{!domainFlow && error && <p className="error-msg">{error}</p>}
				{!domainFlow && success && <p className="success-msg">{success}</p>}
			</div>

			<hr className="divider" />

			<div>
				<p className="section-header">Add domain</p>
				<div style={{ display: "flex", gap: 8 }}>
					<button
						type="button"
						className="btn-primary"
						onClick={() => openDomainFlow("generate")}
					>
						Generate Domain
					</button>
					<button
						type="button"
						className="btn-secondary"
						onClick={() => openDomainFlow("custom")}
					>
						Custom Domain
					</button>
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
