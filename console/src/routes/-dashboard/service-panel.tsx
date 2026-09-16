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
import {
	type FormEvent,
	useCallback,
	useEffect,
	useRef,
	useState,
} from "react";
import { cn } from "#/lib/cn";
import type {
	DashboardHomeState,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";
import { panelIconBtn } from "#/lib/ui-classes";
import { deploymentStagesForDisplay } from "./deployment-inline";
import { PanelDeployments } from "./panel-deployments";
import { withSourceStage } from "./panel-deployments-helpers";
import { doUpdateService } from "./server-fns";
import {
	formatError,
	serviceHealth,
	serviceStatusLabel,
} from "./service-utils";
import type { DashboardTab } from "./types";
import { usePulseDelay } from "./use-pulse-delay";

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
	onServiceDeleted,
	onSpecSaveStateChange,
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
	onServiceDeleted: (serviceId: string) => void;
	onSpecSaveStateChange?: (key: string, saving: boolean) => void;
}) {
	const currentService = service;
	const build = currentService.latestBuild ?? service.latestBuild;
	const health = serviceHealth(currentService);
	const hasUndeployedChanges =
		(currentService.unappliedChangeCount ??
			(currentService.pendingChanges ? 1 : 0)) > 0;
	const stages = withSourceStage(
		currentService,
		build,
		deploymentStagesForDisplay(
			currentService.latestDeployment,
			build,
			build?.stages ?? [],
		),
	);
	const prevHealthRef = useRef(health);
	const [heroVisible, setHeroVisible] = useState(health !== "healthy");
	const [heroExiting, setHeroExiting] = useState(false);
	const [heroCompleting, setHeroCompleting] = useState(false);
	const [, setPanelModulesVersion] = useState(0);
	const [seedVariableKey, setSeedVariableKey] = useState<string>();
	const reportVariablesSaving = useCallback(
		(saving: boolean) => onSpecSaveStateChange?.("variables", saving),
		[onSpecSaveStateChange],
	);

	useEffect(() => {
		const prevHealth = prevHealthRef.current;
		prevHealthRef.current = health;

		if (health === "healthy" && prevHealth !== "healthy") {
			setHeroCompleting(true);
			const t1 = setTimeout(() => {
				setHeroCompleting(false);
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

	const pulseDelay = usePulseDelay(health === "building");

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
			<div className="flex h-header shrink-0 items-center gap-1.5 border-b border-line bg-[linear-gradient(180deg,rgba(255,255,255,0.02),transparent),rgba(24,23,21,0.92)] px-4 py-2">
				<EditableServiceHeaderName
					service={currentService}
					project={project}
					onSaved={onServiceUpdated}
				/>
				<span className="flex-1" />
				{heroVisible && !hasUndeployedChanges && (
					<output
						aria-label="Service status"
						className={cn(
							"relative flex h-[26px] min-w-24 shrink-0 items-center gap-1.5 overflow-hidden border px-2",
							heroExiting
								? "animate-panel-badge-exit"
								: "animate-panel-badge-enter",
							health === "building"
								? "border-[rgba(192,133,32,0.28)] bg-[rgba(192,133,32,0.07)]"
								: health === "failed"
									? "border-[rgba(184,66,66,0.26)] bg-[rgba(184,66,66,0.06)]"
									: "border-line bg-white/[0.025]",
						)}
					>
						{heroCompleting && (
							<span className="pointer-events-none absolute inset-0 animate-hero-glimmer bg-gradient-to-r from-transparent via-white/14 to-transparent" />
						)}
						<span
							className={cn(
								"font-condensed text-[11px] font-bold tracking-[0.09em] whitespace-nowrap uppercase",
								health === "building"
									? "text-building"
									: health === "failed"
										? "text-failed"
										: health === "healthy"
											? "text-healthy"
											: "text-muted",
							)}
						>
							{serviceStatusLabel(currentService)}
						</span>
						{health === "building" && stages.length > 0 && (
							<div
								className="grid h-[3px] w-[54px] shrink-0 auto-cols-fr grid-flow-col gap-0.5"
								role="img"
								aria-label="Deploy progress"
							>
								{stages.map((stage) => {
									const segmentState =
										health === "building" &&
										stage.state === "DEPLOYMENT_STAGE_STATE_SUCCEEDED"
											? "building-done"
											: stage.state;
									return (
										<span
											key={stage.key || stage.label}
											className={cn(
												"rounded-full transition-[background-color] duration-300",
												heroCompleting ||
													segmentState === "DEPLOYMENT_STAGE_STATE_SUCCEEDED"
													? "bg-healthy"
													: segmentState === "building-done" ||
															segmentState === "DEPLOYMENT_STAGE_STATE_RUNNING"
														? "bg-building"
														: segmentState === "DEPLOYMENT_STAGE_STATE_FAILED"
															? "bg-failed"
															: "bg-[rgba(80,76,71,0.7)]",
												!heroCompleting &&
													segmentState === "DEPLOYMENT_STAGE_STATE_RUNNING" &&
													"animate-pulse-building",
											)}
											style={
												!heroCompleting &&
												stage.state === "DEPLOYMENT_STAGE_STATE_RUNNING"
													? { animationDelay: pulseDelay }
													: undefined
											}
											title={stage.label}
										/>
									);
								})}
							</div>
						)}
					</output>
				)}
				<button
					type="button"
					className={panelIconBtn}
					onClick={onRefresh}
					title="Refresh service"
				>
					<RefreshCw size={13} />
				</button>
				<button
					type="button"
					className={panelIconBtn}
					onClick={onClose}
					title="Close service panel"
				>
					<X size={14} />
				</button>
			</div>

			<div className="flex min-h-[54px] shrink-0 items-end gap-[22px] overflow-x-auto border-b border-line bg-[linear-gradient(180deg,rgba(255,255,255,0.035),transparent),rgba(20,18,16,0.88)] px-4 pt-2.5">
				{tabs.map((tab) => (
					<button
						type="button"
						key={tab.id}
						className={cn(
							"inline-flex h-[34px] shrink-0 cursor-pointer items-center gap-1.5 border-0 border-b-2 bg-transparent px-0.5 pb-2.5 font-condensed text-xs font-bold tracking-[0.12em] whitespace-nowrap uppercase transition-[color,border-color] duration-100",
							activeTab === tab.id
								? "border-accent text-accent"
								: "border-transparent text-dim hover:text-ink",
						)}
						onClick={() => onTabChange(tab.id)}
					>
						{tab.icon}
						<span>{tab.label}</span>
					</button>
				))}
			</div>

			<div className="relative min-h-0 flex-1 overflow-hidden">
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
									domains={state.domainBindings}
									autoDeploy={state.environment?.autoDeploy ?? true}
									onOpenVariables={(key) => {
										setSeedVariableKey(key);
										onTabChange("variables");
									}}
									onRedeployed={(next) => {
										onServiceUpdated(next.service);
										onRefresh();
									}}
								/>
							)}
							{activeTab === "variables" &&
								project &&
								(VariablesPanel ? (
									<div className="flex h-full flex-col gap-4 overflow-y-auto px-[18px] pt-[18px] pb-7">
										{}
										<VariablesPanel
											service={service}
											seedKey={seedVariableKey}
											onSaved={onServiceUpdated}
											onSavingChange={reportVariablesSaving}
										/>
									</div>
								) : (
									<PanelTabFallback />
								))}
							{activeTab === "settings" &&
								project &&
								(SettingsPanel ? (
									<div className="flex h-full flex-col gap-4 overflow-y-auto px-[18px] pt-[18px] pb-7">
										<SettingsPanel
											service={currentService}
											state={state}
											onSaved={onServiceUpdated}
											onDeleted={onServiceDeleted}
											onSavingChange={onSpecSaveStateChange}
										/>
									</div>
								) : (
									<PanelTabFallback />
								))}
							{activeTab === "domains" &&
								project &&
								(DomainsPanel ? (
									<div className="flex h-full flex-col gap-4 overflow-y-auto px-[18px] pt-[18px] pb-7">
										<DomainsPanel
											service={service}
											state={state}
											status={status}
										/>
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
		<div className="flex h-full flex-col gap-4 overflow-y-auto px-[18px] pt-[18px] pb-7">
			<div className="flex items-center gap-1.5 text-[13px] text-muted">
				<Loader2 size={13} className="animate-spin" />
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
	const formRef = useRef<HTMLFormElement>(null);

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

	useEffect(() => {
		if (!editing) return;
		const onPointerDown = (event: MouseEvent) => {
			const root = formRef.current;
			if (!root || root.contains(event.target as Node)) return;
			setDraftName(service.name);
			setError(undefined);
			setEditing(false);
		};
		document.addEventListener("mousedown", onPointerDown);
		return () => document.removeEventListener("mousedown", onPointerDown);
	}, [editing, service.name]);

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
			<form
				ref={formRef}
				className="flex h-8 min-w-0 max-w-[min(520px,calc(100%-72px))] flex-1 items-center gap-0 border border-[rgba(92,170,112,0.42)] bg-[linear-gradient(180deg,rgba(255,255,255,0.035),transparent),rgba(15,14,13,0.86)] p-0.5 shadow-[inset_0_-1px_0_rgba(92,170,112,0.2),0_0_0_1px_rgba(15,14,13,0.8)]"
				onSubmit={saveName}
			>
				<input
					ref={inputRef}
					className="h-full min-w-[120px] flex-1 appearance-none rounded-none border-0 bg-transparent px-[9px] font-display text-[22px] font-medium tracking-[-0.03em] text-ink caret-accent shadow-none outline-none selection:bg-[rgba(92,170,112,0.28)]"
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
					className="inline-flex h-full w-[30px] cursor-pointer items-center justify-center border-0 border-l border-[rgba(80,76,71,0.65)] bg-transparent text-muted transition-[color,background-color] duration-100 enabled:hover:bg-white/[0.045] enabled:hover:text-accent enabled:focus-visible:bg-white/[0.045] enabled:focus-visible:text-accent enabled:focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-40"
					disabled={saving || draftName.trim() === ""}
					title="Save service name"
				>
					{saving ? (
						<Loader2 size={13} className="animate-spin" />
					) : (
						<Check size={14} />
					)}
				</button>
				<button
					type="button"
					className="inline-flex h-full w-[30px] cursor-pointer items-center justify-center border-0 border-l border-[rgba(80,76,71,0.65)] bg-transparent text-muted transition-[color,background-color] duration-100 enabled:hover:bg-white/[0.045] enabled:hover:text-failed enabled:focus-visible:bg-white/[0.045] enabled:focus-visible:text-failed enabled:focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-40"
					onClick={cancelEditing}
					disabled={saving}
					title="Cancel service name edit"
				>
					<X size={14} />
				</button>
				{error && (
					<span className="min-w-0 overflow-hidden text-[11px] text-ellipsis whitespace-nowrap text-failed">
						{error}
					</span>
				)}
			</form>
		);
	}

	return (
		<button
			type="button"
			className="inline-flex h-7 max-w-[min(420px,calc(100%-72px))] min-w-0 cursor-pointer items-center overflow-hidden border border-transparent border-b-[rgba(255,255,255,0.12)] bg-transparent p-0 text-ink enabled:hover:border-b-accent enabled:hover:bg-white/[0.025] enabled:focus-visible:border-b-accent enabled:focus-visible:bg-white/[0.025] enabled:focus-visible:outline-none disabled:cursor-default"
			onClick={startEditing}
			disabled={!project}
			title="Rename service"
		>
			<span className="max-w-full min-w-0 overflow-hidden font-display text-[22px] font-medium tracking-[-0.03em] text-ellipsis whitespace-nowrap">
				{service.name}
			</span>
		</button>
	);
}
