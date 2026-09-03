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
import { errorMsg } from "#/lib/ui-classes";

import {
	isInProgressDeploymentState,
	partitionDeployments,
} from "./deployment-inline";
import { DeploymentCard, DeploymentHistoryRow } from "./panel-deployment-cards";
import { DeploymentLogsView } from "./panel-deployment-logs";
import {
	compareDeploymentsNewestFirst,
	createDeploymentRecord,
	deploymentRecordKey,
	deploymentSubtitle,
	deploymentTitle,
	formatError,
	hasActiveDeployment,
	hasDeploymentIdentity,
	hydrateDeploymentRecord,
	isSameDeploymentRecord,
	mergeDeploymentRecords,
	newIdempotencyKey,
	shouldRenderDeploymentHistoryEntry,
} from "./panel-deployments-helpers";
import { doApplyDeploymentAction, fetchServiceDeployments } from "./server-fns";
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
	onOpenVariables,
	onRedeployed,
}: {
	service: DashboardServiceRecord;
	status: DashboardServiceStatus | null;
	project: DashboardProject | undefined;
	domains?: DashboardDomainBinding[];
	onOpenVariables?: (key: string) => void;
	onRedeployed?: (status: DashboardServiceStatus) => void;
}) {
	const currentService = status?.service ?? service;
	const deploymentStatus =
		currentService.latestDeployment ?? service.latestDeployment;
	const build = currentService.latestBuild ?? service.latestBuild;
	const allocation = status?.allocation;
	const publicDomain = domains.find(
		(binding) =>
			binding.serviceId === currentService.id &&
			binding.ownershipState !== "DOMAIN_OWNERSHIP_STATE_UNVERIFIED",
	)?.hostname;
	const replicaCount = currentService.desiredReplicaCount ?? 1;
	const rolloutGeneration =
		deploymentStatus?.rolloutGeneration ||
		allocation?.desiredRolloutGeneration ||
		currentService.rolloutGeneration;
	const [deployments, setDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [sessionDeployments, setSessionDeployments] = useState<
		Array<DashboardDeploymentRecord>
	>([]);
	const [deploymentsError, setDeploymentsError] = useState<string>();
	const [logTarget, setLogTarget] = useState<DeploymentLogTarget | null>(null);
	const [historyOpen, setHistoryOpen] = useState(false);
	const [nowMs, setNowMs] = useState(() => Date.now());
	const lastObservedDeployment = useRef<DashboardDeploymentRecord | null>(null);
	const actionKeys = useRef(new Map<string, string>());

	const loadDeployments = useCallback(async () => {
		if (!project) {
			setDeployments([]);
			setDeploymentsError(undefined);
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
		}
	}, [project, service.id]);

	useEffect(() => {
		void loadDeployments();
	}, [loadDeployments]);

	const applyAction = useCallback(
		async (
			record: DashboardDeploymentRecord,
			action: DashboardDeploymentAction,
			allocationId?: string,
		) => {
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

	useEffect(() => {
		const id = window.setInterval(() => setNowMs(Date.now()), 30_000);
		return () => window.clearInterval(id);
	}, []);

	const currentRecord = useMemo(
		() =>
			createDeploymentRecord({
				serviceId: service.id,
				build,
				allocation,
				rolloutGeneration,
				isCurrent: true,
				status: deploymentStatus,
			}),
		[service.id, build, allocation, rolloutGeneration, deploymentStatus],
	);

	const mergedDeployments = useMemo(
		() =>
			mergeDeploymentRecords([
				...sessionDeployments,
				...deployments,
				currentRecord,
			]),
		[sessionDeployments, deployments, currentRecord],
	);

	const { live: liveDeployments, history: previousDeployments } =
		useMemo(() => {
			const partitioned = partitionDeployments(mergedDeployments);
			return {
				live: partitioned.live.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
				history: partitioned.history.filter((entry) =>
					shouldRenderDeploymentHistoryEntry(entry, currentService),
				),
			};
		}, [mergedDeployments, currentService]);

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
		const previousDeployment = lastObservedDeployment.current;

		if (
			previousDeployment &&
			!isSameDeploymentRecord(previousDeployment, currentRecord) &&
			hasDeploymentIdentity(previousDeployment)
		) {
			setSessionDeployments((currentSessionDeployments) =>
				mergeDeploymentRecords([
					...currentSessionDeployments,
					{
						...previousDeployment,
						isCurrent: false,
					},
				]),
			);
		}

		lastObservedDeployment.current = currentRecord;
	}, [currentRecord]);

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
				<div className="flex flex-col gap-2.5">
					{liveDeployments.map((entry) => (
						<DeploymentCard
							key={deploymentRecordKey(entry)}
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
									id: deploymentRecordKey(entry),
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
										key={deploymentRecordKey(entry)}
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
												id: deploymentRecordKey(entry),
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
			</div>

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

				{logTarget && project && (
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
		</div>
	);
}
