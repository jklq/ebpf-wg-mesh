import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";
import {
	doDeleteService,
	doDiscardServiceChanges,
	doReleaseEnvironment,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import {
	type ApplyingServiceChanges,
	applyingChangeKey,
	hasUnappliedChanges,
	mergeApplyingServices,
	queuedChangeCount,
	snapshotApplyingChanges,
	unappliedChangeCount,
} from "./dashboard-unapplied";
export function useEnvironmentRelease({
	services,
	environmentId,
	revision,
	mergeStatusService,
	mergeServiceRecord,
	handleServiceDeleted,
	onRefresh,
}: {
	services: DashboardServiceRecord[];
	environmentId: string | null;
	revision: string;
	mergeStatusService: (
		status: DashboardServiceStatus,
		revision: string,
	) => void;
	mergeServiceRecord: (
		service: DashboardServiceRecord,
		revision: string,
	) => void;
	handleServiceDeleted: (id: string) => void;
	onRefresh: () => void;
}) {
	const [deployingChanges, setDeployingChanges] = useState(false);
	const [applyingServices, setApplyingServices] = useState<
		Array<ApplyingServiceChanges>
	>([]);
	const [deployError, setDeployError] = useState<string>();
	const [showChangeDetails, setShowChangeDetails] = useState(false);
	const [discardingChangeId, setDiscardingChangeId] = useState<string>();
	const [pendingSpecWrites, setPendingSpecWrites] = useState<Set<string>>(
		() => new Set(),
	);

	const servicesRef = useRef(services);
	servicesRef.current = services;
	const revisionRef = useRef(revision);
	revisionRef.current = revision;
	const environmentIdRef = useRef(environmentId);
	environmentIdRef.current = environmentId;
	const pendingSpecWritesRef = useRef(pendingSpecWrites);
	pendingSpecWritesRef.current = pendingSpecWrites;
	const specWriteWaitersRef = useRef<Array<() => void>>([]);
	const dirtyServices = services.filter(hasUnappliedChanges);
	const changeSignature = dirtyServices
		.flatMap((service) =>
			(service.unappliedChanges ?? []).map(
				(change) =>
					`${service.id}:${change.id}:${change.action}:${change.newValue}`,
			),
		)
		.sort()
		.join("|");
	const totalUnappliedChanges = dirtyServices.reduce(
		(total, service) => total + unappliedChangeCount(service),
		0,
	);
	const applyingChangeKeys = useMemo(
		() =>
			new Set(
				applyingServices.flatMap((service) =>
					service.changeIds.map((changeId) =>
						applyingChangeKey(service.serviceId, changeId),
					),
				),
			),
		[applyingServices],
	);
	const applyingChangeCount = applyingServices.reduce(
		(total, service) => total + service.count,
		0,
	);
	const deployableUnappliedChanges = dirtyServices.reduce(
		(total, service) => total + queuedChangeCount(service, applyingChangeKeys),
		0,
	);
	const hasPendingSpecWrites = pendingSpecWrites.size > 0;
	const showPrompt =
		deployableUnappliedChanges > 0 ||
		applyingChangeCount > 0 ||
		hasPendingSpecWrites ||
		Boolean(deployError);

	const setSpecWriteState = useCallback(
		(serviceId: string, key: string, saving: boolean) => {
			const writeKey = `${serviceId}:${key}`;
			setPendingSpecWrites((current) => {
				if (current.has(writeKey) === saving) return current;
				const next = new Set(current);
				if (saving) next.add(writeKey);
				else next.delete(writeKey);
				return next;
			});
		},
		[],
	);
	useEffect(() => {
		if (hasPendingSpecWrites) return;
		const waiters = specWriteWaitersRef.current.splice(0);
		for (const resolve of waiters) resolve();
	}, [hasPendingSpecWrites]);

	useEffect(() => {
		if (totalUnappliedChanges === 0) {
			setDeployError(undefined);
		}
	}, [totalUnappliedChanges]);

	const deployServiceBatch = async (
		servicesToDeploy: Array<DashboardServiceRecord>,
		currentEnvironmentId: string,
	) => {
		const applying = servicesToDeploy.map(snapshotApplyingChanges);
		const basisRevision = revisionRef.current;
		setDeployError(undefined);
		setShowChangeDetails(false);
		setDeployingChanges(true);
		setApplyingServices((current) => mergeApplyingServices(current, applying));
		try {
			const statuses = await doReleaseEnvironment({
				data: { environmentId: currentEnvironmentId },
			});
			for (const status of statuses) {
				mergeStatusService(status, basisRevision);
			}
			onRefresh();
		} catch (error) {
			setDeployError(`Deploy stopped: ${formatError(error)}`);
		} finally {
			setApplyingServices((current) =>
				current.filter(
					(service) =>
						!applying.some(
							(applied) => applied.serviceId === service.serviceId,
						),
				),
			);
			setDeployingChanges(false);
		}
	};

	const handleDeployChanges = async () => {
		if (deployingChanges) return;
		if (!environmentId) return;
		setDeployingChanges(true);
		if (pendingSpecWritesRef.current.size > 0) {
			await new Promise<void>((resolve) => {
				specWriteWaitersRef.current.push(resolve);
			});
		}
		const currentEnvironmentId = environmentIdRef.current;
		const currentServices = servicesRef.current.filter(hasUnappliedChanges);
		if (!currentEnvironmentId || currentServices.length === 0) {
			setDeployingChanges(false);
			return;
		}
		await deployServiceBatch(currentServices, currentEnvironmentId);
	};

	const handleDiscardServiceChanges = async (serviceId: string) => {
		if (discardingChangeId) return;
		setDiscardingChangeId(`service:${serviceId}`);
		const basisRevision = revisionRef.current;
		try {
			const currentService = servicesRef.current.find(
				(service) => service.id === serviceId,
			);
			const hasOtherUndeployedServices = servicesRef.current.some(
				(service) => service.id !== serviceId && hasUnappliedChanges(service),
			);
			const rolloutGeneration =
				currentService?.rolloutGeneration ??
				currentService?.latestDeployment?.rolloutGeneration ??
				"0";
			if (currentService && rolloutGeneration === "0") {
				await doDeleteService({ data: { serviceId } });
				handleServiceDeleted(serviceId);
				if (!hasOtherUndeployedServices) setShowChangeDetails(false);
				return;
			}
			const service = await doDiscardServiceChanges({
				data: { serviceId, discardAll: true },
			});
			mergeServiceRecord(service, basisRevision);
			if (!hasOtherUndeployedServices && !hasUnappliedChanges(service)) {
				setShowChangeDetails(false);
			}
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
		} finally {
			setDiscardingChangeId(undefined);
		}
	};

	const handleDiscardChange = async (serviceId: string, changeId: string) => {
		if (discardingChangeId) return;
		setDiscardingChangeId(`${serviceId}:${changeId}`);
		const basisRevision = revisionRef.current;
		try {
			const service = await doDiscardServiceChanges({
				data: { serviceId, changeIds: [changeId] },
			});
			mergeServiceRecord(service, basisRevision);
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
		} finally {
			setDiscardingChangeId(undefined);
		}
	};

	return {
		dirtyServices,
		changeSignature,
		totalUnappliedChanges,
		applyingChangeCount,
		deployableUnappliedChanges,
		hasPendingSpecWrites,
		showPrompt,
		deployingChanges,
		deployError,
		showChangeDetails,
		setShowChangeDetails,
		discardingChangeId,
		setSpecWriteState,
		handleDeployChanges,
		handleDiscardServiceChanges,
		handleDiscardChange,
	};
}
