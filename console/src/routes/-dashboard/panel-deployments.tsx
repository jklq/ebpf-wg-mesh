import { ArrowLeft, Boxes, EyeOff, Globe2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import type {
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDomainBinding,
	DashboardProject,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

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
			binding.ownershipState !== "unverified",
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
			entry.build?.state === "queued" ||
			entry.build?.state === "running",
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
		<div className="deployments-panel">
			<div className="deployments-list">
				<div className="deployments-summary">
					<div className="deployments-summary-item">
						{publicDomain ? <Globe2 size={15} /> : <EyeOff size={15} />}
						<span>{publicDomain ?? "Unexposed service"}</span>
					</div>
					<div className="deployments-summary-item">
						<Boxes size={15} />
						<span>
							{replicaCount} {replicaCount === 1 ? "Replica" : "Replicas"}
						</span>
					</div>
				</div>
				<div className="deployment-live-list">
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
					<section className="deployment-history">
						<button
							type="button"
							className="deployment-history-toggle"
							aria-expanded={historyOpen}
							onClick={() => setHistoryOpen((open) => !open)}
						>
							<span className="deployment-history-chevron" aria-hidden>
								{historyOpen ? "▾" : "▸"}
							</span>
							History
							<span className="deployment-history-count">
								{previousDeployments.length}
							</span>
						</button>
						{historyOpen && (
							<div className="deployment-history-list">
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
					<div className="deployment-error compact">{deploymentsError}</div>
				)}
			</div>

			<div className={`deployment-log-drawer ${logTarget ? "open" : ""}`}>
				<div className="deployment-log-drawer-header">
					<button
						type="button"
						className="deployment-log-back"
						onClick={() => setLogTarget(null)}
					>
						<ArrowLeft size={14} />
						Back
					</button>
					<div className="deployment-log-drawer-copy">
						<div className="deployment-log-drawer-title">Deployment logs</div>
						{logTarget?.title && (
							<div className="deployment-log-drawer-subtitle">
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
