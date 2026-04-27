import {
	ArrowUpRight,
	Check,
	Globe,
	KeyRound,
	Layers,
	Loader2,
	RefreshCw,
	Settings,
	X,
} from "lucide-react";
import { type FormEvent, useEffect, useRef, useState } from "react";

import type {
	DashboardHomeState,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { PanelDeployments } from "./panel-deployments";
import { PanelDomains } from "./panel-domains";
import { PanelSettings } from "./panel-settings";
import { PanelVariables } from "./panel-variables";
import { doUpdateService } from "./server-fns";
import {
	buildServiceURL,
	formatError,
	heroStatusLabel,
	serviceHealth,
	shortSha,
} from "./service-utils";
import type { DashboardTab } from "./types";

const tabs = [
	{ id: "deployments", label: "Deployments", icon: <Layers size={11} /> },
	{ id: "variables", label: "Variables", icon: <KeyRound size={11} /> },
	{ id: "domains", label: "Domains", icon: <Globe size={11} /> },
	{ id: "settings", label: "Settings", icon: <Settings size={11} /> },
] as const;

export function ServicePanel({
	service,
	status,
	project,
	state,
	activeTab,
	onTabChange,
	onClose,
	onRefresh,
	onServiceUpdated,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
	state: DashboardHomeState;
	activeTab: DashboardTab;
	onTabChange: (tab: DashboardTab) => void;
	onClose: () => void;
	onRefresh: () => void;
	onServiceUpdated: (service: DashboardServiceRecord) => void;
}) {
	const currentService = status?.service ?? service;
	const build = currentService.latestBuild ?? service.latestBuild;
	const health = serviceHealth(currentService);
	const stages = build?.stages ?? [];
	const domainBindings = state.domainBindings.filter(
		(binding) => binding.serviceId === service.id,
	);
	const primaryDomain = domainBindings[0];
	const serviceURL = primaryDomain
		? buildServiceURL(state, primaryDomain.hostname)
		: null;
	const statusMeta = buildStatusMeta(build, status?.allocation);
	const runningStageCount = stages.filter(
		(stage) => stage.state === "running" || stage.state === "succeeded",
	).length;

	// Hero visibility — hidden when healthy, animated out when transitioning to healthy
	const prevHealthRef = useRef(health);
	const [heroVisible, setHeroVisible] = useState(health !== "healthy");
	const [heroExiting, setHeroExiting] = useState(false);
	const [heroCompleting, setHeroCompleting] = useState(false);

	useEffect(() => {
		const prevHealth = prevHealthRef.current;
		prevHealthRef.current = health;

		if (health === "healthy" && prevHealth !== "healthy") {
			// Phase 1: flash all segments green + glimmer sweep (~750ms)
			setHeroCompleting(true);
			const t1 = setTimeout(() => {
				setHeroCompleting(false);
				// Phase 2: slide the hero out
				setHeroExiting(true);
			}, 750);
			const t2 = setTimeout(() => {
				setHeroVisible(false);
				setHeroExiting(false);
			}, 750 + 450);
			return () => {
				clearTimeout(t1);
				clearTimeout(t2);
			};
		}
		if (health !== "healthy") {
			setHeroVisible(true);
			setHeroExiting(false);
			setHeroCompleting(false);
		}
	}, [health]);

	// Synchronise all pulsing elements rendered in the same pass to the same
	// wall-clock animation phase so the status dot and running rail segments
	// always beat together, regardless of when each element first mounted.
	// -(Date.now() % period) places the element at the correct point in the
	// cycle right now; if the delay changes on a later render the restart is
	// seamless because it re-targets the same current phase.
	const pulseDelay =
		health === "building" ? `${-(Date.now() % 1400)}ms` : "0ms";

	return (
		<>
			<div className="panel-crumbs">
				<EditableServiceHeaderName
					service={currentService}
					project={project}
					onSaved={onServiceUpdated}
				/>
				<span style={{ flex: 1 }} />
				<button
					type="button"
					className="panel-icon-btn"
					onClick={onRefresh}
					title="Refresh service"
				>
					<RefreshCw size={13} />
				</button>
				<button
					type="button"
					className="panel-icon-btn"
					onClick={onClose}
					title="Close service panel"
				>
					<X size={14} />
				</button>
			</div>

			{heroVisible && (
				<div
					className={`panel-hero tone-${health}${heroCompleting ? " hero-completing" : ""}${heroExiting ? " hero-exiting" : ""}`}
				>
					<div className="panel-hero-row">
						<h1 className="panel-service-name">{currentService.name}</h1>
						{serviceURL && primaryDomain && (
							<a
								href={serviceURL}
								target="_blank"
								rel="noreferrer"
								className="panel-service-link"
							>
								<Globe size={11} />
								<span>{primaryDomain.hostname}</span>
								<ArrowUpRight size={11} />
							</a>
						)}
					</div>

					<div className="panel-status-strip">
						<span
							className={`status-dot ${health} panel-status-dot`}
							style={{ animationDelay: pulseDelay }}
						/>
						<span className="panel-status-label">{heroStatusLabel(build)}</span>
						<div className="panel-status-meta">
							{build?.commitSha && (
								<span className="mono">{shortSha(build.commitSha)}</span>
							)}
							{build?.commitSha && statusMeta.length > 0 && (
								<span className="panel-inline-dot" />
							)}
							{statusMeta.map((entry, index) => (
								<span key={entry} className="panel-status-meta-item">
									{index > 0 && <span className="panel-inline-dot" />}
									{entry}
								</span>
							))}
						</div>
					</div>

					{stages.length > 0 && (
						<div
							className="panel-rollout-rail"
							role="img"
							aria-label="Deployment progress"
						>
							{stages.map((stage) => {
								// While the overall build is in progress, colour already-succeeded
								// segments with the building tone so the rail is one uniform colour.
								// They only flip to green during the hero-completing flash.
								const segmentState =
									health === "building" && stage.state === "succeeded"
										? "building-done"
										: stage.state;
								return (
									<span
										key={stage.key || stage.label}
										className={`panel-rollout-segment ${segmentState}`}
										style={
											stage.state === "running"
												? { animationDelay: pulseDelay }
												: undefined
										}
										title={stage.label}
									/>
								);
							})}
						</div>
					)}

					{build?.commitMessage && (
						<p className="panel-hero-commit">{build.commitMessage}</p>
					)}
					{build?.commitAuthor && (
						<p className="panel-hero-subtle">
							<span>{build.commitAuthor}</span>
							{currentService.spec?.source?.trackedRef && (
								<>
									<span className="panel-inline-dot" />
									<span className="mono">
										{currentService.spec.source.trackedRef}
									</span>
								</>
							)}
							{stages.length > 0 && (
								<>
									<span className="panel-inline-dot" />
									<span>{`${runningStageCount}/${stages.length} stages`}</span>
								</>
							)}
						</p>
					)}
				</div>
			)}

			<div className="tab-bar condensed">
				{tabs.map((tab) => (
					<button
						type="button"
						key={tab.id}
						className={`tab-item ${activeTab === tab.id ? "active" : ""}`}
						onClick={() => onTabChange(tab.id)}
					>
						{tab.icon}
						<span>{tab.label}</span>
						{tab.id === "domains" && domainBindings.length > 0 ? (
							<span className="tab-item-count">{domainBindings.length}</span>
						) : null}
					</button>
				))}
			</div>

			<div className="service-panel-content">
				{activeTab === "deployments" && (
					<PanelDeployments
						key={service.id}
						service={service}
						status={status}
						project={project}
					/>
				)}
				{activeTab === "variables" && project && (
					<div className="service-panel-scroll">
						<PanelVariables
							service={service}
							project={project}
							onSaved={(updated) => {
								onServiceUpdated(updated);
								onRefresh();
							}}
						/>
					</div>
				)}
				{activeTab === "settings" && project && (
					<div className="service-panel-scroll">
						<PanelSettings
							service={service}
							project={project}
							state={state}
							onSaved={(updated) => {
								onServiceUpdated(updated);
								onRefresh();
							}}
						/>
					</div>
				)}
				{activeTab === "domains" && project && (
					<div className="service-panel-scroll">
						<PanelDomains service={service} project={project} state={state} />
					</div>
				)}
			</div>
		</>
	);
}

function EditableServiceHeaderName({
	service,
	project,
	onSaved,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject | undefined;
	onSaved: (service: DashboardServiceRecord) => void;
}) {
	const [editing, setEditing] = useState(false);
	const [draftName, setDraftName] = useState(service.name);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const inputRef = useRef<HTMLInputElement>(null);

	useEffect(() => {
		if (!editing) {
			setDraftName(service.name);
			setError(undefined);
		}
	}, [editing, service.name]);

	useEffect(() => {
		if (editing) {
			inputRef.current?.focus();
			inputRef.current?.select();
		}
	}, [editing]);

	const startEditing = () => {
		setDraftName(service.name);
		setError(undefined);
		setEditing(true);
	};

	const cancelEditing = () => {
		setDraftName(service.name);
		setError(undefined);
		setEditing(false);
	};

	const saveName = async (event: FormEvent) => {
		event.preventDefault();
		if (!project || saving) return;
		const nextName = draftName.trim();
		if (!nextName || nextName === service.name) {
			cancelEditing();
			return;
		}
		const previousService = service;
		const optimisticService: DashboardServiceRecord = {
			...service,
			name: nextName,
			updatedAt: new Date(),
		};
		setSaving(true);
		setError(undefined);
		setEditing(false);
		onSaved(optimisticService);
		try {
			const updated = await doUpdateService({
				data: {
					projectId: project.id,
					serviceId: service.id,
					serviceName: nextName,
				},
			});
			onSaved(updated);
		} catch (e) {
			onSaved(previousService);
			setDraftName(nextName);
			setError(formatError(e));
			setEditing(true);
		} finally {
			setSaving(false);
		}
	};

	if (editing) {
		return (
			<form className="panel-title-edit" onSubmit={saveName}>
				<input
					ref={inputRef}
					className="panel-title-input"
					value={draftName}
					onChange={(event) => setDraftName(event.target.value)}
					onKeyDown={(event) => {
						if (event.key === "Escape") {
							event.preventDefault();
							cancelEditing();
						}
					}}
					aria-label="Service name"
				/>
				<button
					type="submit"
					className="panel-title-action save"
					disabled={saving || draftName.trim() === ""}
					title="Save service name"
				>
					{saving ? (
						<Loader2
							size={13}
							style={{ animation: "spin 1s linear infinite" }}
						/>
					) : (
						<Check size={14} />
					)}
				</button>
				<button
					type="button"
					className="panel-title-action cancel"
					onClick={cancelEditing}
					disabled={saving}
					title="Cancel service name edit"
				>
					<X size={14} />
				</button>
				{error && <span className="panel-title-error">{error}</span>}
			</form>
		);
	}

	return (
		<button
			type="button"
			className="panel-title-button"
			onClick={startEditing}
			disabled={!project}
			title="Rename service"
		>
			<span className="panel-title-name">{service.name}</span>
		</button>
	);
}

function buildStatusMeta(
	build: DashboardServiceRecord["latestBuild"],
	allocation: DashboardServiceStatus["allocation"],
): string[] {
	const parts: string[] = [];
	const activeStages = build?.stages?.filter(
		(stage) => stage.state === "running" || stage.state === "succeeded",
	).length;
	if (build?.stages?.length) {
		parts.push(`${activeStages ?? 0}/${build.stages.length} stages`);
	}
	const startedAt =
		build?.startedAt ??
		build?.queuedAt ??
		build?.finishedAt ??
		allocation?.updatedAt;
	if (startedAt) {
		parts.push(formatRelativeAge(startedAt));
	}
	return parts;
}

function formatRelativeAge(date: Date): string {
	const diffMs = date.getTime() - Date.now();
	const absSeconds = Math.round(Math.abs(diffMs) / 1000);
	const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

	if (absSeconds < 60) {
		return rtf.format(Math.round(diffMs / 1000), "second");
	}

	const absMinutes = Math.round(absSeconds / 60);
	if (absMinutes < 60) {
		return rtf.format(Math.round(diffMs / 60_000), "minute");
	}

	const absHours = Math.round(absMinutes / 60);
	if (absHours < 24) {
		return rtf.format(Math.round(diffMs / 3_600_000), "hour");
	}

	return rtf.format(Math.round(diffMs / 86_400_000), "day");
}
