import {
	CircleAlert,
	CircleCheck,
	Globe,
	Loader2,
	Pencil,
	Trash2,
	X,
} from "lucide-react";
import type { RefObject } from "react";

import type {
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import { buildServiceURL } from "./service-utils";
import { ModalOverlay, PanelSection } from "./ui";

export function DomainPanelView({
	state,
	domainFlow,
	platformBinding,
	targetPortId,
	targetPortRef,
	targetPort,
	setTargetPort,
	setError,
	recommendedPort,
	hostnameId,
	hostnameRef,
	hostname,
	setHostname,
	error,
	success,
	primaryDomainActionRef,
	generating,
	publishing,
	setDomainFlow,
	handlePublish,
	handleGenerate,
	editingBinding,
	setEditingBinding,
	handleEditSave,
	editPortRef,
	editPort,
	setEditPort,
	setEditError,
	editError,
	editSaving,
	deleteConfirm,
	setDeleteConfirm,
	deletingHostname,
	handleDelete,
	removeDomainRef,
	internalHostname,
	internalShortName,
	loadingBindings,
	visibleBindings,
	pendingDomain,
	openEdit,
	openDomainFlow,
}: {
	state: DashboardHomeState;
	domainFlow: "generate" | "custom" | null;
	platformBinding: DashboardDomainBinding | undefined;
	targetPortId: string;
	targetPortRef: RefObject<HTMLInputElement | null>;
	targetPort: string;
	setTargetPort: (value: string) => void;
	setError: (value: string | undefined) => void;
	recommendedPort: number;
	hostnameId: string;
	hostnameRef: RefObject<HTMLInputElement | null>;
	hostname: string;
	setHostname: (value: string) => void;
	error: string | undefined;
	success: string | undefined;
	primaryDomainActionRef: RefObject<HTMLButtonElement | null>;
	generating: boolean;
	publishing: boolean;
	setDomainFlow: (value: "generate" | "custom" | null) => void;
	handlePublish: () => Promise<void>;
	handleGenerate: (opts: { keepFlowOpen: boolean }) => Promise<void>;
	editingBinding: DashboardDomainBinding | null;
	setEditingBinding: (value: DashboardDomainBinding | null) => void;
	handleEditSave: () => Promise<void>;
	editPortRef: RefObject<HTMLInputElement | null>;
	editPort: string;
	setEditPort: (value: string) => void;
	setEditError: (value: string | undefined) => void;
	editError: string | undefined;
	editSaving: boolean;
	deleteConfirm: string | null;
	setDeleteConfirm: (value: string | null) => void;
	deletingHostname: string | null;
	handleDelete: (hostname: string) => Promise<void>;
	removeDomainRef: RefObject<HTMLButtonElement | null>;
	internalHostname: string;
	internalShortName: string;
	loadingBindings: boolean;
	visibleBindings: DashboardDomainBinding[];
	pendingDomain: { hostname?: string } | null;
	openEdit: (binding: DashboardDomainBinding) => void;
	openDomainFlow: (flow: "generate" | "custom") => void;
}) {
	return (
		<div className="domains-stack">
			{domainFlow && (
				<ModalOverlay
					ariaLabel={
						domainFlow === "generate" ? "Generate domain" : "Custom domain"
					}
					onClose={() => setDomainFlow(null)}
				>
					<form
						className="modal-card"
						onSubmit={(event) => {
							event.preventDefault();
							if (domainFlow === "custom" && platformBinding) {
								if (!publishing && hostname.trim()) void handlePublish();
								return;
							}
							if (!generating) {
								void handleGenerate({ keepFlowOpen: domainFlow === "custom" });
							}
						}}
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
									ref={targetPortRef}
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
										ref={hostnameRef}
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
									type="submit"
									ref={primaryDomainActionRef}
									className="btn-primary"
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
									type="submit"
									className="btn-primary"
									disabled={publishing || hostname.trim() === ""}
								>
									{publishing && (
										<Loader2
											size={12}
											style={{ animation: "spin 1s linear infinite" }}
										/>
									)}
									Add domain
								</button>
							)}
						</div>
					</form>
				</ModalOverlay>
			)}

			{editingBinding && (
				<ModalOverlay
					ariaLabel="Edit domain"
					onClose={() => setEditingBinding(null)}
				>
					<form
						className="modal-card"
						onSubmit={(event) => {
							event.preventDefault();
							void handleEditSave();
						}}
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
								ref={editPortRef}
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
								type="submit"
								className="btn-primary"
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
					</form>
				</ModalOverlay>
			)}

			{deleteConfirm && (
				<ModalOverlay
					ariaLabel="Remove domain"
					onClose={() => setDeleteConfirm(null)}
				>
					<form
						className="modal-card"
						onSubmit={(event) => {
							event.preventDefault();
							if (!deletingHostname) void handleDelete(deleteConfirm);
						}}
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
								type="submit"
								ref={removeDomainRef}
								className="btn-danger"
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
					</form>
				</ModalOverlay>
			)}

			<PanelSection
				title="Private mesh"
				lede="Reach this service from anything else in the current environment."
			>
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
			</PanelSection>

			<PanelSection
				title="Public domains"
				lede="Hostnames that route into this service."
			>
				<div className="domain-list">
					{loadingBindings && (
						<div
							style={{
								display: "flex",
								gap: 6,
								color: "var(--text-muted)",
								fontSize: 13,
							}}
						>
							<Loader2
								size={13}
								style={{ animation: "spin 1s linear infinite" }}
							/>
							Loading…
						</div>
					)}
					{!loadingBindings &&
						visibleBindings.length === 0 &&
						!pendingDomain && (
							<div className="domain-empty">No public domains yet.</div>
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
					{visibleBindings.map((binding) => {
						const pending = pendingDomain?.hostname === binding.hostname;
						const unverified =
							!binding.platformGenerated &&
							binding.ownershipState !== "verified";
						return (
							<div
								key={binding.hostname}
								className={`domain-item ${pending ? "domain-item-pending" : ""} ${unverified ? "domain-item-unverified" : ""}`}
							>
								<div className="domain-item-body">
									<div className="domain-item-row">
										<div
											style={{
												display: "flex",
												alignItems: "center",
												gap: 6,
												overflow: "hidden",
											}}
										>
											{pending || unverified ? (
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
											{!pending && (
												<span
													className={`domain-ownership-badge ${unverified ? "unverified" : "verified"}`}
												>
													{unverified ? "Waiting for CNAME" : "Live"}
												</span>
											)}
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
									{unverified && (
										<div className="domain-ownership-detail">
											<p>
												<CircleAlert size={12} aria-hidden="true" />
												{formatOwnershipMessage(
													binding.ownershipMessage,
													platformBinding?.hostname,
												)}
											</p>
											{platformBinding && (
												<p className="domain-cname-hint">
													{binding.hostname} CNAME {platformBinding.hostname}
												</p>
											)}
										</div>
									)}
								</div>
							</div>
						);
					})}
					{!domainFlow && error && <p className="error-msg">{error}</p>}
					{!domainFlow && success && <p className="success-msg">{success}</p>}
				</div>
				<div className="panel-sticky-actions">
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
			</PanelSection>
		</div>
	);
}

export function formatOwnershipMessage(
	message: string | undefined,
	platformHostname: string | undefined,
): string {
	const expected = platformHostname
		? `Point a CNAME at ${platformHostname}.`
		: "Point a CNAME at the generated platform hostname.";
	if (!message) {
		return expected;
	}
	if (message.includes("no such host") || message.includes("lookup ")) {
		return `DNS does not resolve yet. ${expected}`;
	}
	const mismatch = /expected ([^,]+), got (.+)$/.exec(message);
	if (mismatch) {
		return `CNAME currently points to ${mismatch[2]}. Expected ${mismatch[1]}.`;
	}
	return message;
}

export function recommendedTargetPort(
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
