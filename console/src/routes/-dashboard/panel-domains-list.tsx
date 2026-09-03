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
import { cn } from "#/lib/cn";
import type {
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import {
	btnDanger,
	btnGhost,
	btnPrimary,
	btnSecondary,
	errorMsg,
	fieldInput,
	fieldLabel,
	modalCard,
	successMsg,
} from "#/lib/ui-classes";

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
		<div className="flex flex-col gap-7">
			{domainFlow && (
				<ModalOverlay
					ariaLabel={
						domainFlow === "generate" ? "Generate domain" : "Custom domain"
					}
					onClose={() => setDomainFlow(null)}
				>
					<form
						className={cn(modalCard, "flex flex-col gap-4 p-6")}
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
					>
						<div className="flex items-center justify-between">
							<span className="text-sm font-semibold text-ink">
								{domainFlow === "generate"
									? "Generate Domain"
									: "Custom Domain"}
							</span>
							<button
								type="button"
								className={cn(btnGhost, "px-1.5 py-1")}
								onClick={() => setDomainFlow(null)}
								aria-label="Close"
							>
								<X size={14} />
							</button>
						</div>

						{(domainFlow === "generate" || !platformBinding) && (
							<div>
								<label className={fieldLabel} htmlFor={targetPortId}>
									App port
								</label>
								<input
									id={targetPortId}
									ref={targetPortRef}
									className={fieldInput}
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
								<p className="mt-1.5 mb-0 text-[11px] text-muted">
									The port your app listens on inside the service.
								</p>
							</div>
						)}

						{domainFlow === "generate" && platformBinding && (
							<div className={cn(successMsg, "font-mono")}>
								{platformBinding.hostname}
							</div>
						)}

						{domainFlow === "custom" && platformBinding && (
							<>
								<div>
									<label className={fieldLabel} htmlFor={hostnameId}>
										Custom hostname
									</label>
									<input
										id={hostnameId}
										ref={hostnameRef}
										className={fieldInput}
										value={hostname}
										onChange={(event) => {
											setHostname(event.target.value);
											setError(undefined);
										}}
										placeholder="app.customer.com"
									/>
								</div>
								<div className="bg-surface-raised px-3 py-2.5 font-mono text-[11px] leading-[1.6] text-muted">
									Create this DNS record:
									<br />
									{hostname || "app.customer.com"} CNAME{" "}
									{platformBinding.hostname}
								</div>
							</>
						)}

						{error && <p className={errorMsg}>{error}</p>}
						{success && <p className={successMsg}>{success}</p>}

						<div className="flex justify-end gap-2">
							<button
								type="button"
								className={btnSecondary}
								onClick={() => setDomainFlow(null)}
							>
								Cancel
							</button>
							{(domainFlow === "generate" || !platformBinding) && (
								<button
									type="submit"
									ref={primaryDomainActionRef}
									className={btnPrimary}
									disabled={generating}
								>
									{generating && <Loader2 size={12} className="animate-spin" />}
									{domainFlow === "custom"
										? "Generate & continue"
										: "Generate Domain"}
								</button>
							)}
							{domainFlow === "custom" && platformBinding && (
								<button
									type="submit"
									className={btnPrimary}
									disabled={publishing || hostname.trim() === ""}
								>
									{publishing && <Loader2 size={12} className="animate-spin" />}
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
						className={cn(modalCard, "flex flex-col gap-4 p-6")}
						onSubmit={(event) => {
							event.preventDefault();
							void handleEditSave();
						}}
					>
						<div className="flex items-center justify-between">
							<span className="text-sm font-semibold text-ink">
								Edit domain
							</span>
							<button
								type="button"
								className={cn(btnGhost, "px-1.5 py-1")}
								onClick={() => setEditingBinding(null)}
							>
								<X size={14} />
							</button>
						</div>
						<div>
							<p className="mb-3 text-xs text-muted">
								<span className="font-mono">{editingBinding.hostname}</span>
							</p>
							<label className={fieldLabel} htmlFor="edit-port">
								App port
							</label>
							<input
								id="edit-port"
								ref={editPortRef}
								className={fieldInput}
								value={editPort}
								onChange={(e) => {
									setEditPort(e.target.value);
									setEditError(undefined);
								}}
								placeholder="8080"
								inputMode="numeric"
							/>
						</div>
						{editError && <p className={errorMsg}>{editError}</p>}
						<div className="flex justify-end gap-2">
							<button
								type="button"
								className={btnSecondary}
								onClick={() => setEditingBinding(null)}
							>
								Cancel
							</button>
							<button
								type="submit"
								className={btnPrimary}
								disabled={editSaving || editPort.trim() === ""}
							>
								{editSaving && <Loader2 size={12} className="animate-spin" />}
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
						className={cn(modalCard, "flex flex-col gap-4 p-6")}
						onSubmit={(event) => {
							event.preventDefault();
							if (!deletingHostname) void handleDelete(deleteConfirm);
						}}
					>
						<span className="text-sm font-semibold text-ink">
							Remove domain?
						</span>
						<p className="m-0 text-[13px] text-muted">
							<span className="font-mono">{deleteConfirm}</span> will stop
							routing traffic immediately.
						</p>
						<div className="flex justify-end gap-2">
							<button
								type="button"
								className={btnSecondary}
								onClick={() => setDeleteConfirm(null)}
								disabled={deletingHostname === deleteConfirm}
							>
								Cancel
							</button>
							<button
								type="submit"
								ref={removeDomainRef}
								className={btnDanger}
								disabled={deletingHostname === deleteConfirm}
							>
								{deletingHostname === deleteConfirm && (
									<Loader2 size={12} className="animate-spin" />
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
				<div className="flex items-center gap-3 border border-[rgba(109,190,130,0.28)] bg-[linear-gradient(180deg,rgba(109,190,130,0.08),transparent_55%),var(--color-surface-raised)] p-3.5">
					<CircleCheck
						size={20}
						aria-hidden="true"
						className="shrink-0 text-healthy"
					/>
					<div className="min-w-0">
						<div className="flex flex-wrap items-center gap-[7px] font-mono text-[13px] text-ink">
							<span>{internalHostname}</span>
							<span className="bg-accent-dim px-[5px] py-0.5 font-sans text-[10px] font-semibold text-accent">
								IPv6
							</span>
						</div>
						<p className="mt-[5px] mb-0 text-[11px] text-muted">
							Ready to talk privately · You can also simply call me{" "}
							<code className="bg-accent-dim px-1 py-px font-mono text-accent">
								{internalShortName}
							</code>
						</p>
					</div>
				</div>
			</PanelSection>

			<PanelSection
				title="Public domains"
				lede="Hostnames that route into this service."
			>
				<div className="flex flex-col gap-2">
					{loadingBindings && (
						<div className="flex gap-1.5 text-[13px] text-muted">
							<Loader2 size={13} className="animate-spin" />
							Loading…
						</div>
					)}
					{!loadingBindings &&
						visibleBindings.length === 0 &&
						!pendingDomain && (
							<div className="border border-dashed border-line-bright px-4 py-[18px] text-[13px] text-muted">
								No public domains yet.
							</div>
						)}
					{pendingDomain && !pendingDomain.hostname && (
						<div className="flex items-center justify-between gap-2 border border-dashed border-line bg-surface-raised px-3.5 py-3 text-muted">
							<div className="flex items-center gap-1.5 overflow-hidden">
								<Loader2 size={12} className="animate-spin" />
								<span className="font-mono text-[13px] text-muted">
									Generating domain…
								</span>
							</div>
						</div>
					)}
					{visibleBindings.map((binding) => {
						const pending = pendingDomain?.hostname === binding.hostname;
						const unverified =
							!binding.platformGenerated &&
							binding.ownershipState !== "DOMAIN_OWNERSHIP_STATE_VERIFIED";
						return (
							<div
								key={binding.hostname}
								className={cn(
									"flex items-center justify-between gap-2 border bg-surface-raised px-3.5 py-3",
									pending || unverified
										? "border-dashed border-line text-muted"
										: "border-line",
								)}
							>
								<div className="flex min-w-0 flex-1 flex-col gap-2">
									<div className="flex items-center justify-between gap-2">
										<div className="flex items-center gap-1.5 overflow-hidden">
											{pending || unverified ? (
												<Loader2 size={12} className="animate-spin" />
											) : (
												<Globe size={12} className="shrink-0 text-healthy" />
											)}
											<span className="overflow-hidden font-mono text-[13px] text-ellipsis whitespace-nowrap text-ink">
												{binding.hostname}
												<span className="text-muted">
													{" "}
													-&gt; :{binding.targetPort}
												</span>
											</span>
										</div>
										<div className="flex shrink-0 items-center gap-1">
											{!pending && (
												<span
													className={cn(
														"text-[10px] font-semibold tracking-[0.04em] whitespace-nowrap uppercase",
														unverified ? "text-accent" : "text-healthy",
													)}
												>
													{unverified ? "Waiting for CNAME" : "Live"}
												</span>
											)}
											<a
												href={buildServiceURL(state, binding.hostname)}
												target="_blank"
												rel="noreferrer"
												className={cn(btnGhost, "text-[11px]")}
											>
												Open ↗
											</a>
											<button
												type="button"
												className={cn(btnGhost, "px-1.5 py-1")}
												onClick={() => openEdit(binding)}
												title="Edit"
											>
												<Pencil size={12} />
											</button>
											<button
												type="button"
												className={cn(btnGhost, "px-1.5 py-1 text-failed")}
												onClick={() => setDeleteConfirm(binding.hostname)}
												title="Remove"
											>
												<Trash2 size={12} />
											</button>
										</div>
									</div>
									{unverified && (
										<div className="flex flex-col gap-1">
											<p className="m-0 flex items-start gap-1.5 text-[11px] leading-[1.45] text-muted">
												<CircleAlert size={12} aria-hidden="true" />
												{formatOwnershipMessage(
													binding.ownershipMessage,
													platformBinding?.hostname,
												)}
											</p>
											{platformBinding && (
												<p className="border border-line bg-surface px-2.5 py-2 font-mono text-ink">
													{binding.hostname} CNAME {platformBinding.hostname}
												</p>
											)}
										</div>
									)}
								</div>
							</div>
						);
					})}
					{!domainFlow && error && <p className={errorMsg}>{error}</p>}
					{!domainFlow && success && <p className={successMsg}>{success}</p>}
				</div>
				<div className="flex flex-wrap items-center gap-x-3.5 gap-y-2.5 pt-0.5">
					<button
						type="button"
						className={btnPrimary}
						onClick={() => openDomainFlow("generate")}
					>
						Generate Domain
					</button>
					<button
						type="button"
						className={btnSecondary}
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
	const healthy = [
		...(state.serviceStatus?.allocation?.healthyIpv4Ports ?? []),
		...(state.serviceStatus?.allocation?.healthyIpv6Ports ?? []),
	].find(
		(port) => Number.isInteger(port) && port >= 1 && port <= 65535,
	);
	return healthy ?? 8080;
}
