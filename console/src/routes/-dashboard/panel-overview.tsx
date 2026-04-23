import { Globe, Loader2 } from "lucide-react";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import {
	buildBadgeClass,
	buildServiceURL,
	healthLabel,
	serviceHealth,
	shortId,
	shortSha,
} from "./service-utils";
import { InfoRow, MonoValue } from "./ui";

export function PanelOverview({
	service,
	status,
	loading,
	state,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	loading: boolean;
	state: DashboardHomeState;
}) {
	const alloc = status?.allocation;
	const build = service.latestBuild;
	const health = serviceHealth(service);
	const domainBinding = state.domainBindings.find(
		(binding) => binding.serviceId === service.id,
	);
	const serviceURL = domainBinding
		? buildServiceURL(state, domainBinding.hostname)
		: null;
	const runtimePorts = service.spec?.runtime.ports ?? [];
	const source = service.spec?.source;

	return (
		<div style={{ display: "flex", flexDirection: "column", gap: 20 }}>
			{serviceURL && (
				<a
					href={serviceURL}
					target="_blank"
					rel="noreferrer"
					style={{
						display: "flex",
						alignItems: "center",
						gap: 8,
						padding: "10px 14px",
						background: "var(--accent-dim)",
						border: "1px solid var(--accent)",
						borderRadius: 1,
						fontSize: 13,
						color: "var(--accent)",
						fontWeight: 600,
						overflow: "hidden",
					}}
				>
					<Globe size={14} style={{ flexShrink: 0 }} />
					<span
						style={{
							overflow: "hidden",
							textOverflow: "ellipsis",
							whiteSpace: "nowrap",
							flex: 1,
						}}
					>
						{serviceURL}
					</span>
					<span style={{ fontSize: 10, flexShrink: 0 }}>↗</span>
				</a>
			)}

			<div>
				<p className="section-header">Runtime</p>
				<div>
					<InfoRow label="Status">
						{loading ? (
							<Loader2
								size={12}
								style={{ animation: "spin 1s linear infinite" }}
							/>
						) : (
							<span className={`badge ${health}`}>{healthLabel(health)}</span>
						)}
					</InfoRow>
					{alloc?.phase && (
						<InfoRow label="Phase">
							<span>{alloc.phase}</span>
						</InfoRow>
					)}
					{alloc?.allocationIp && (
						<InfoRow label="Allocation IP">
							<MonoValue>{alloc.allocationIp}</MonoValue>
						</InfoRow>
					)}
					{alloc?.message && !alloc.healthy && (
						<InfoRow label="Message">
							<span style={{ color: "var(--failed)", fontSize: 12 }}>
								{alloc.message}
							</span>
						</InfoRow>
					)}
					{runtimePorts.length > 0 && (
						<InfoRow label="Listen ports">
							<MonoValue>
								{runtimePorts
									.map(
										(port) => `${port.port}${port.primary ? " primary" : ""}`,
									)
									.join(", ")}
							</MonoValue>
						</InfoRow>
					)}
					{alloc?.healthyPorts && alloc.healthyPorts.length > 0 && (
						<InfoRow label="Healthy ports">
							<MonoValue>{alloc.healthyPorts.join(", ")}</MonoValue>
						</InfoRow>
					)}
				</div>
			</div>

			{build && (
				<div>
					<p className="section-header">Latest build</p>
					<div>
						<InfoRow label="State">
							<span className={`badge ${buildBadgeClass(build.state)}`}>
								{build.state}
							</span>
						</InfoRow>
						{build.commitSha && (
							<InfoRow label="Commit">
								<MonoValue>{shortSha(build.commitSha)}</MonoValue>
							</InfoRow>
						)}
						{build.imageDigest && (
							<InfoRow label="Image">
								<MonoValue title={build.imageDigest}>
									{build.imageDigest.slice(0, 20)}…
								</MonoValue>
							</InfoRow>
						)}
						{build.failureReason && (
							<InfoRow label="Error">
								<span style={{ color: "var(--failed)", fontSize: 12 }}>
									{build.failureReason}
								</span>
							</InfoRow>
						)}
					</div>
				</div>
			)}

			{source && (
				<div>
					<p className="section-header">Source</p>
					<div>
						<InfoRow label="Repository">
							<MonoValue>{source.repositorySelector}</MonoValue>
						</InfoRow>
						<InfoRow label="Branch">
							<MonoValue>{source.trackedRef}</MonoValue>
						</InfoRow>
						{source.buildRecipe?.dockerfilePath && (
							<InfoRow label="Dockerfile">
								<MonoValue>{source.buildRecipe.dockerfilePath}</MonoValue>
							</InfoRow>
						)}
					</div>
				</div>
			)}

			<div>
				<p className="section-header">Identifiers</p>
				<div>
					<InfoRow label="Service ID">
						<MonoValue>{shortId(service.id)}</MonoValue>
					</InfoRow>
					<InfoRow label="Project ID">
						<MonoValue>{shortId(service.projectId)}</MonoValue>
					</InfoRow>
				</div>
			</div>
		</div>
	);
}
