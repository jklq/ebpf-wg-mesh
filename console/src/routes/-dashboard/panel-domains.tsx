import { Globe, Loader2 } from "lucide-react";
import { useEffect, useState } from "react";

import type {
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardProject,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

import {
	doCheckDNS,
	doCreateDomainBinding,
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
	const [bindings, setBindings] = useState<DashboardDomainBinding[]>([]);
	const [loadingBindings, setLoadingBindings] = useState(true);
	const [hostname, setHostname] = useState("");
	const [dnsResult, setDnsResult] = useState<{
		state: string;
		instruction: string;
	} | null>(null);
	const [checking, setChecking] = useState(false);
	const [publishing, setPublishing] = useState(false);
	const [error, setError] = useState<string>();
	const [success, setSuccess] = useState<string>();

	useEffect(() => {
		setLoadingBindings(true);
		fetchDomainBindings({
			data: { projectId: project.id, serviceId: service.id },
		})
			.then(setBindings)
			.catch(() => setBindings([]))
			.finally(() => setLoadingBindings(false));
	}, [project.id, service.id]);

	const handleCheckDNS = async () => {
		if (!hostname.trim()) return;
		setError(undefined);
		setDnsResult(null);
		setChecking(true);
		try {
			const result = await doCheckDNS({ data: { hostname: hostname.trim() } });
			if (result) {
				setDnsResult({ state: result.state, instruction: result.instruction });
			}
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

	return (
		<div style={{ display: "flex", flexDirection: "column", gap: 20 }}>
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
							</span>
						</div>
						<a
							href={buildServiceURL(state, binding.hostname)}
							target="_blank"
							rel="noreferrer"
							className="btn-ghost"
							style={{ fontSize: 11, flexShrink: 0 }}
						>
							Open ↗
						</a>
					</div>
				))}
			</div>

			<hr className="divider" />

			<div>
				<p className="section-header">Add domain</p>

				<div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
					<div>
						<label className="field-label">Hostname</label>
						<input
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

					{state.ingressTargetHost && (
						<div
							style={{
								fontSize: 11,
								color: "var(--text-muted)",
								padding: "8px 10px",
								background: "var(--surface-raised)",
								borderRadius: 6,
								fontFamily: "var(--font-mono)",
								lineHeight: 1.5,
							}}
						>
							Set a CNAME record: {hostname || "<hostname>"} →{" "}
							{state.ingressTargetHost}
						</div>
					)}

					{dnsResult && (
						<div
							className={
								dnsResult.state === "verified" ? "success-msg" : "error-msg"
							}
						>
							{dnsResult.instruction}
						</div>
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
							Check DNS
						</button>

						{dnsResult?.state === "verified" && (
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
