import { ArrowLeft, Boxes, EyeOff, Globe2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { cn } from "#/lib/cn";
import type {
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDomainBinding,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";
import { btnSecondary, errorMsg } from "#/lib/ui-classes";

import {
	isInProgressDeploymentState,
	partitionDeployments,
} from "./deployment-inline";
import { DeploymentCard, DeploymentHistoryRow } from "./panel-deployment-cards";
import { DeploymentLogsView } from "./panel-deployment-logs";
import {
	compareDeploymentsNewestFirst,
	deploymentSubtitle,
	deploymentTitle,
	formatError,
	hasActiveDeployment,
	hydrateDeploymentRecord,
	newIdempotencyKey,
	pendingManualDeployRevision,
	reconcileCurrentDeployment,
	shouldRenderDeploymentHistoryEntry,
} from "./panel-deployments-helpers";
import {
	doApplyDeploymentAction,
	doReleaseEnvironment,
	fetchServiceDeployments,
} from "./server-fns";
import { shortSha } from "./service-utils";
import { usePolling } from "./use-polling";

type DeploymentLogTarget = {
	id: string;
	title: string;
	subtitle?: string;
	build: DashboardDeploymentRecord["build"];
	allocation: DashboardDeploymentRecord["allocation"];
	rolloutGeneration?: number;
	active: boolean;
};

export function PanelDeployments({
	service,
	status,
	project,
	domains = [],
	autoDeploy = true,
	onOpenVariables,
	onRedeployed,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
	domains?: DashboardDomainBinding[];
	autoDeploy?: boolean;
	onOpenVariables?: (key: string) => void;
	onRedeployed?: (status: DashboardServiceStatus) => void;
}) {
	const currentService = status?.service ?? service;
	const publicDomain = domains.find(
		(binding) =>
			binding.serviceId === currentService.id &&
			binding.ownershipState !== "DOMAIN_OWNERSHIP_STATE_UNVERIFIED",
	)?.hostname;
	const replicaCount = currentService.desiredReplicaCount ?? 1;
	const [deployments, setDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [deploymentsError, setDeploymentsError] = useState<string>();
	const [deploymentsLoading, setDeploymentsLoading] = useState(true);
	const [logTarget, setLogTarget] = useState<DeploymentLogTarget | null>(null);
	const [historyOpen, setHistoryOpen] = useState(false);
	const [nowMs, setNowMs] = useState(() => Date.now());
	const [deployingRevision, setDeployingRevision] = useState(false);
	const [revisionDeployError, setRevisionDeployError] = useState<string>();
	const pendingRevision = pendingManualDeployRevision(
		currentService,
		autoDeploy,
	);
	const actionKeys = useRef(new Map<string, string>());
	const deploymentActivityKey = [
		currentService.latestBuild?.buildId,
		currentService.latestBuild?.state,
		currentService.latestDeployment?.state,
		currentService.latestDeployment?.transitionedAt?.getTime(),
	].join(":");

	const loadDeployments = useCallback(async () => {
		if (!project) {
			setDeployments([]);
			setDeploymentsError(undefined);
			setDeploymentsLoading(false);
			return;
		}
		try {
			const nextDeployments = await fetchServiceDeployments({
				data: {
					serviceId: service.id,
					limit: 10,
				},
			});
			setDeployments(
				Array.isArray(nextDeployments)
					? nextDeployments
							.map(hydrateDeploymentRecord)
							.sort(compareDeploymentsNewestFirst)
					: [],
			);
			setDeploymentsError(undefined);
		} catch (cause) {
			setDeployments([]);
			setDeploymentsError(formatError(cause, "Unable to load deployments."));
		} finally {
			setDeploymentsLoading(false);
		}
	}, [project, service.id]);

	useEffect(() => {
		// A status event is the invalidation signal for this service's history.
		void deploymentActivityKey;
		void loadDeployments();
	}, [deploymentActivityKey, loadDeployments]);

	const applyAction = useCallback(
		async (
			record: DashboardDeploymentRecord,
			action: DashboardDeploymentAction,
			allocationId?: string,
		) => {
			if (
				action === "DEPLOYMENT_ACTION_ROLLBACK" ||
				action === "DEPLOYMENT_ACTION_EXACT_REDEPLOY"
			) {
				const image =
					record.artifact?.imageRef ?? record.build?.artifact?.imageRef;
				if (
					!window.confirm(
						`${action === "DEPLOYMENT_ACTION_ROLLBACK" ? "Roll back" : "Redeploy"}${image ? ` using ${image}` : " this deployment"}? Pinned secret versions will be restored.`,
					)
				)
					return;
			}
			const requestKey = `${record.id}:${action}:${allocationId ?? "all"}`;
			let idempotencyKey = actionKeys.current.get(requestKey);
			if (!idempotencyKey) {
				idempotencyKey = newIdempotencyKey();
				actionKeys.current.set(requestKey, idempotencyKey);
			}
			const next = await doApplyDeploymentAction({
				data: {
					serviceId: service.id,
					deploymentId: record.id,
					action,
					idempotencyKey,
					allocationId,
				},
			});
			actionKeys.current.delete(requestKey);
			onRedeployed?.(next);
			await loadDeployments();
		},
		[service.id, loadDeployments, onRedeployed],
	);

	const deployRevision = useCallback(async () => {
		if (deployingRevision) return;
		setDeployingRevision(true);
		setRevisionDeployError(undefined);
		try {
			const statuses = await doReleaseEnvironment({
				data: { environmentId: currentService.environmentId },
			});
			const next = statuses.find(
				(entry) => entry.service.id === currentService.id,
			);
			if (next) onRedeployed?.(next);
			await loadDeployments();
		} catch (cause) {
			setRevisionDeployError(formatError(cause, "Unable to deploy."));
		} finally {
			setDeployingRevision(false);
		}
	}, [
		currentService.environmentId,
		currentService.id,
		deployingRevision,
		loadDeployments,
		onRedeployed,
	]);

	useEffect(() => {
		const id = window.setInterval(() => setNowMs(Date.now()), 30_000);
		return () => window.clearInterval(id);
	}, []);

	const visibleDeployments = useMemo(
		() => reconcileCurrentDeployment(deployments, currentService),
		[deployments, currentService],
	);
	const { live: liveDeployments, history: previousDeployments } =
		useMemo(() => {
			const partitioned = partitionDeployments(visibleDeployments);
			return {
				live: partitioned.live.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
				history: partitioned.history.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
			};
		}, [visibleDeployments, currentService]);

	const shouldPollDeployments = liveDeployments.some(
		(entry) =>
			isInProgressDeploymentState(entry.status?.state) ||
			hasActiveDeployment(entry.status, entry.build),
	);
	const deploymentInProgress = liveDeployments.some(
		(entry) =>
			isInProgressDeploymentState(entry.status?.state) ||
			entry.build?.state === "BUILD_STATE_QUEUED" ||
			entry.build?.state === "BUILD_STATE_RUNNING",
	);

	usePolling(loadDeployments, {
		enabled: shouldPollDeployments,
		intervalMs: 5000,
	});

	useEffect(() => {
		if (!logTarget) return;
		const onKeyDown = (event: KeyboardEvent) => {
			if (event.key !== "Escape") return;
			event.preventDefault();
			setLogTarget(null);
		};
		document.addEventListener("keydown", onKeyDown, { capture: true });
		return () =>
			document.removeEventListener("keydown", onKeyDown, { capture: true });
	}, [logTarget]);

	useEffect(() => {
		if (!project && logTarget) setLogTarget(null);
	}, [project, logTarget]);

	return (
		<div className="relative h-full overflow-hidden">
			<div className="flex h-full flex-col gap-2.5 overflow-y-auto p-4">
				<div className="flex min-h-7 items-center justify-between gap-4 text-xs text-muted">
					<div className="flex min-w-0 items-center gap-[7px] overflow-hidden">
						{publicDomain ? (
							<Globe2 size={15} className="shrink-0" />
						) : (
							<EyeOff size={15} className="shrink-0" />
						)}
						<span className="overflow-hidden text-ellipsis whitespace-nowrap">
							{publicDomain ?? "Unexposed service"}
						</span>
					</div>
					<div className="flex min-w-0 items-center gap-[7px]">
						<Boxes size={15} className="shrink-0" />
						<span className="overflow-hidden text-ellipsis whitespace-nowrap">
							{replicaCount} {replicaCount === 1 ? "Replica" : "Replicas"}
						</span>
					</div>
				</div>
				{pendingRevision && (
					<output
						aria-label="Latest commit is waiting for manual deploy"
						className={cn(
							"flex min-h-7 flex-wrap items-center gap-x-2 gap-y-1.5 rounded-sm border px-2.5 py-1.5 text-xs leading-[1.35]",
							"border-[rgba(80,76,71,0.55)] bg-[rgba(15,14,13,0.42)] text-muted",
						)}
					>
						<span className="min-w-0 flex-1">
							Auto-deploy is off —{" "}
							<span className="font-mono text-ink">
								{shortSha(pendingRevision.commitSha)}
							</span>{" "}
							is waiting.
						</span>
						<button
							type="button"
							className={cn(btnSecondary, "min-h-7 px-[11px]")}
							disabled={deployingRevision}
							onClick={() => void deployRevision()}
						>
							{deployingRevision ? "Deploying…" : "Deploy now"}
						</button>
					</output>
				)}
				{revisionDeployError && (
					<div className={cn(errorMsg, "px-[9px] py-[7px] text-[11px]")}>
						{revisionDeployError}
					</div>
				)}
				<div className="flex flex-col gap-2.5">
					{liveDeployments.map((entry) => (
						<DeploymentCard
							key={entry.id}
							service={currentService}
							record={entry}
							logsEnabled={Boolean(project)}
							allocations={status?.allocations ?? []}
							nowMs={nowMs}
							deploymentInProgress={deploymentInProgress}
							onOpenVariables={onOpenVariables}
							onAction={applyAction}
							onOpenLogs={() =>
								setLogTarget({
									id: entry.id,
									title: deploymentTitle(entry.build, entry.isCurrent),
									subtitle: deploymentSubtitle(
										entry.build,
										entry.rolloutGeneration,
										nowMs,
									),
									build: entry.build,
									allocation: entry.allocation,
									rolloutGeneration: entry.rolloutGeneration,
									active: hasActiveDeployment(entry.status, entry.build),
								})
							}
						/>
					))}
				</div>

				{previousDeployments.length > 0 && (
					<section className="flex shrink-0 flex-col">
						<button
							type="button"
							className="inline-flex w-full cursor-pointer items-center gap-2 border-0 bg-transparent p-0 text-left font-display text-base font-medium tracking-[-0.02em] text-ink"
							aria-expanded={historyOpen}
							onClick={() => setHistoryOpen((open) => !open)}
						>
							<span className="w-2.5 text-xs text-dim" aria-hidden>
								{historyOpen ? "▾" : "▸"}
							</span>
							History
							<span className="font-mono text-[11px] font-normal text-dim">
								{previousDeployments.length}
							</span>
						</button>
						{historyOpen && (
							<div className="flex flex-col">
								{previousDeployments.map((entry) => (
									<DeploymentHistoryRow
										serviceId={service.id}
										key={entry.id}
										build={entry.build}
										allocation={entry.allocation}
										status={entry.status}
										record={entry}
										logsEnabled={Boolean(project)}
										nowMs={nowMs}
										deploymentInProgress={deploymentInProgress}
										onAction={applyAction}
										onOpenLogs={() =>
											setLogTarget({
												id: entry.id,
												title: deploymentTitle(entry.build, false),
												subtitle: deploymentSubtitle(
													entry.build,
													entry.rolloutGeneration,
													nowMs,
												),
												build: entry.build,
												allocation: entry.allocation,
												rolloutGeneration: entry.rolloutGeneration,
												active: hasActiveDeployment(entry.status, entry.build),
											})
										}
									/>
								))}
							</div>
						)}
					</section>
				)}

				{deploymentsError && (
					<div className={cn(errorMsg, "px-[9px] py-[7px] text-[11px]")}>
						{deploymentsError}
					</div>
				)}

				{!deploymentsLoading &&
					!deploymentsError &&
					liveDeployments.length === 0 &&
					previousDeployments.length === 0 && (
						<div
							className="flex min-h-48 flex-1 items-center justify-center border border-line px-6 text-center"
							style={{
								backgroundImage:
									"radial-gradient(circle, rgba(126,119,110,0.32) 1px, transparent 1px)",
								backgroundSize: "14px 14px",
							}}
						>
							<div className="bg-surface px-4 py-3">
								<div className="font-display text-base font-medium text-ink">
									Nothing deployed yet
								</div>
								<div className="mt-1 text-xs text-muted">
									{currentService.pendingChanges
										? "Deploy your changes to create the first deployment."
										: "Deployment activity will appear here."}
								</div>
							</div>
						</div>
					)}
			</div>

			{project && (
				<div
					aria-hidden={logTarget ? undefined : true}
					className={cn(
						"absolute inset-0 z-[2] flex flex-col border-l border-line bg-surface transition-transform duration-200 ease-out",
						logTarget ? "translate-x-0" : "translate-x-[102%]",
					)}
				>
					<div className="flex shrink-0 items-start gap-3 border-b border-line p-4 max-[900px]:flex-col max-[900px]:items-stretch">
						<button
							type="button"
							className="inline-flex min-h-8 cursor-pointer items-center gap-1.5 border border-line bg-transparent px-2.5 font-condensed text-[11px] font-bold uppercase tracking-[0.09em] text-ink hover:bg-surface-hover"
							onClick={() => setLogTarget(null)}
						>
							<ArrowLeft size={14} />
							Back
						</button>
						<div className="min-w-0 flex-1">
							<div className="font-condensed text-sm font-bold uppercase tracking-[0.08em] text-ink">
								Deployment logs
							</div>
							{logTarget?.title && (
								<div className="mt-1 text-xs leading-[1.4] text-muted">
									{logTarget.title}
									{logTarget.subtitle ? ` • ${logTarget.subtitle}` : ""}
								</div>
							)}
						</div>
					</div>

					{logTarget && (
						<DeploymentLogsView
							service={currentService}
							project={project}
							build={logTarget.build}
							allocation={logTarget.allocation}
							rolloutGeneration={logTarget.rolloutGeneration}
							active={logTarget.active}
						/>
					)}
				</div>
			)}
		</div>
	);
}
