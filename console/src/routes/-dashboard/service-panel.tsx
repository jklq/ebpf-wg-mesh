import {
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
import { doUpdateService } from "./server-fns";
import { formatError, healthLabel, serviceHealth } from "./service-utils";
import type { DashboardTab } from "./types";

type PanelModules = {
	variables?: typeof import("./panel-variables").PanelVariables;
	domains?: typeof import("./panel-domains").PanelDomains;
	settings?: typeof import("./panel-settings").PanelSettings;
};

const loadedPanelModules: PanelModules = {};
let preloadPanelTabsPromise: Promise<void> | null = null;

const loadPanelVariables = async () => {
	const module = await import("./panel-variables");
	loadedPanelModules.variables = module.PanelVariables;
};
const loadPanelDomains = async () => {
	const module = await import("./panel-domains");
	loadedPanelModules.domains = module.PanelDomains;
};
const loadPanelSettings = async () => {
	const module = await import("./panel-settings");
	loadedPanelModules.settings = module.PanelSettings;
};

const preloadAllPanelTabs = (): Promise<void> => {
	preloadPanelTabsPromise ??= Promise.all([
		loadPanelVariables(),
		loadPanelDomains(),
		loadPanelSettings(),
	]).then(() => undefined);
	return preloadPanelTabsPromise;
};

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
	// Deploy badge visibility — hidden when healthy, animated out when transitioning to healthy
	const prevHealthRef = useRef(health);
	const [heroVisible, setHeroVisible] = useState(health !== "healthy");
	const [heroExiting, setHeroExiting] = useState(false);
	const [heroCompleting, setHeroCompleting] = useState(false);
	const [, setPanelModulesVersion] = useState(0);

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

	useEffect(() => {
		let cancelled = false;
		const markLoaded = () => {
			if (!cancelled) {
				setPanelModulesVersion((version) => version + 1);
			}
		};
		if (typeof window.requestIdleCallback === "function") {
			const id = window.requestIdleCallback(() => {
				void preloadAllPanelTabs().then(markLoaded);
			});
			return () => {
				cancelled = true;
				window.cancelIdleCallback(id);
			};
		}
		const id = window.setTimeout(() => {
			void preloadAllPanelTabs().then(markLoaded);
		}, 0);
		return () => {
			cancelled = true;
			window.clearTimeout(id);
		};
	}, []);

	return (
		<>
			<div className="panel-crumbs">
				<EditableServiceHeaderName
					service={currentService}
					project={project}
					onSaved={onServiceUpdated}
				/>
				<span style={{ flex: 1 }} />
				{heroVisible && (
					<div
						className={`panel-deploy-badge tone-${health}${heroCompleting ? " completing" : ""}${heroExiting ? " exiting" : ""}`}
					>
						<span
							className="panel-badge-label"
							style={
								health === "building"
									? { animationDelay: pulseDelay }
									: undefined
							}
						>
							{healthLabel(health)}
						</span>
						{stages.length > 0 && (
							<div
								className="panel-badge-rail"
								role="img"
								aria-label="Deploy progress"
							>
								{stages.map((stage) => {
									const segmentState =
										health === "building" && stage.state === "succeeded"
											? "building-done"
											: stage.state;
									return (
										<span
											key={stage.key || stage.label}
											className={`panel-badge-segment ${segmentState}`}
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
					</div>
				)}
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
					</button>
				))}
			</div>

			<div className="service-panel-content">
				{(() => {
					const VariablesPanel = loadedPanelModules.variables;
					const SettingsPanel = loadedPanelModules.settings;
					const DomainsPanel = loadedPanelModules.domains;

					return (
						<>
							{activeTab === "deployments" && (
								<PanelDeployments
									key={service.id}
									service={service}
									status={status}
									project={project}
								/>
							)}
							{activeTab === "variables" &&
								project &&
								(VariablesPanel ? (
									<div className="service-panel-scroll">
										<VariablesPanel
											service={service}
											onSaved={(updated) => {
												onServiceUpdated(updated);
												onRefresh();
											}}
										/>
									</div>
								) : (
									<PanelTabFallback />
								))}
							{activeTab === "settings" &&
								project &&
								(SettingsPanel ? (
									<div className="service-panel-scroll">
										<SettingsPanel
											service={service}
											state={state}
											onSaved={(updated) => {
												onServiceUpdated(updated);
												onRefresh();
											}}
										/>
									</div>
								) : (
									<PanelTabFallback />
								))}
							{activeTab === "domains" &&
								project &&
								(DomainsPanel ? (
									<div className="service-panel-scroll">
										<DomainsPanel service={service} state={state} />
									</div>
								) : (
									<PanelTabFallback />
								))}
						</>
					);
				})()}
			</div>
		</>
	);
}

function PanelTabFallback() {
	return (
		<div className="service-panel-scroll">
			<div className="panel-loading-row">
				<Loader2 size={13} style={{ animation: "spin 1s linear infinite" }} />
			</div>
		</div>
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
