import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
	DashboardServiceRecord,
	DashboardServiceStatus,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import {
	doDeleteResource,
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
	stagedVolumes = [],
	pendingCreationCount = 0,
	environmentId,
	revision,
	mergeStatusService,
	mergeServiceRecord,
	handleServiceDeleted,
	handleVolumesDiscarded,
	onRefresh,
}: {
	services: DashboardServiceRecord[];
	/** Volumes created since the last release; each is one pending change. */
	stagedVolumes?: DashboardVolume[];
	pendingCreationCount?: number;
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
	/** Drops staged volumes the server discarded along with a draft. */
	handleVolumesDiscarded: (volumeIds: Array<string>) => void;
	onRefresh: () => void;
}) {
	const [deployingChanges, setDeployingChanges] = useState(false);
	const [applyingServices, setApplyingServices] = useState<
		Array<ApplyingServiceChanges>
	>([]);
	const [deployError, setDeployError] = useState<string>();
	const [showChangeDetails, setShowChangeDetails] = useState(false);
	const [discardingChangeId, setDiscardingChangeId] = useState<string>();
	const [pendingDiscard, setPendingDiscard] =
		useState<ApplyingServiceChanges>();
	const discardingRef = useRef(false);
	const [pendingSpecWrites, setPendingSpecWrites] = useState<Set<string>>(
		() => new Set(),
	);

	const deployingRef = useRef(false);
	const servicesRef = useRef(services);
	servicesRef.current = services;
	const stagedVolumesRef = useRef(stagedVolumes);
	stagedVolumesRef.current = stagedVolumes;
	const revisionRef = useRef(revision);
	revisionRef.current = revision;
	const environmentIdRef = useRef(environmentId);
	environmentIdRef.current = environmentId;
	const pendingSpecWritesRef = useRef(pendingSpecWrites);
	pendingSpecWritesRef.current = pendingSpecWrites;
	const specWriteWaitersRef = useRef<Array<() => void>>([]);
	const visibleServices = useMemo(
		() =>
			services.map((service) => {
				if (!pendingDiscard || pendingDiscard.serviceId !== service.id)
					return service;
				const count = queuedChangeCount(
					service,
					new Set(pendingDiscard.changeKeys),
				);
				return {
					...service,
					pendingChanges: count > 0,
					unappliedChangeCount: count,
					unappliedChanges: service.unappliedChanges.filter(
						(change) =>
							!pendingDiscard.changeKeys.includes(
								applyingChangeKey(service.id, change.id, change.newValue),
							),
					),
				};
			}),
		[services, pendingDiscard],
	);
	const dirtyServices = visibleServices.filter(hasUnappliedChanges);
	const discardingChangeCount = pendingDiscard?.count ?? 0;
	const changeSignature = dirtyServices
		.flatMap((service) =>
			(service.unappliedChanges ?? []).map(
				(change) =>
					`${service.id}:${change.id}:${change.action}:${change.newValue}`,
			),
		)
		.concat(stagedVolumes.map((volume) => `volume:${volume.id}`))
		.sort()
		.join("|");
	const totalUnappliedChanges =
		stagedVolumes.length +
		dirtyServices.reduce(
			(total, service) => total + unappliedChangeCount(service),
			0,
		);
	const applyingChangeKeys = useMemo(
		() => new Set(applyingServices.flatMap((service) => service.changeKeys)),
		[applyingServices],
	);
	const applyingChangeCount = applyingServices.reduce(
		(total, service) => total + service.count,
		0,
	);
	const deployableUnappliedChanges =
		(deployingChanges ? 0 : stagedVolumes.length) +
		dirtyServices.reduce(
			(total, service) =>
				total + queuedChangeCount(service, applyingChangeKeys),
			0,
		);
	const hasPendingSpecWrites = pendingSpecWrites.size > 0;
	const showPrompt =
		pendingCreationCount > 0 ||
		discardingChangeCount > 0 ||
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
		if (
			discardingRef.current ||
			deployingRef.current ||
			pendingCreationCount > 0 ||
			!environmentId
		)
			return;
		deployingRef.current = true;
		setDeployingChanges(true);
		try {
			if (pendingSpecWritesRef.current.size > 0) {
				await new Promise<void>((resolve) => {
					specWriteWaitersRef.current.push(resolve);
				});
			}
			const currentEnvironmentId = environmentIdRef.current;
			const currentServices = servicesRef.current.filter(hasUnappliedChanges);
			if (
				currentEnvironmentId &&
				(currentServices.length > 0 || stagedVolumesRef.current.length > 0)
			) {
				await deployServiceBatch(currentServices, currentEnvironmentId);
			}
		} finally {
			deployingRef.current = false;
			setDeployingChanges(false);
		}
	};

	// Mirrors the server: discarding a draft takes the staged volume it mounted
	// with it unless another service still mounts that volume.
	const stagedVolumesOnlyMountedBy = (
		serviceId: string,
		volumeName: string | undefined,
	) =>
		volumeName
			? stagedVolumesRef.current
					.filter(
						(volume) =>
							volume.name === volumeName &&
							!servicesRef.current.some(
								(other) =>
									other.id !== serviceId &&
									other.spec?.runtime?.volume?.volumeName === volume.name,
							),
					)
					.map((volume) => volume.id)
			: [];

	const handleDiscardServiceChanges = async (serviceId: string) => {
		if (discardingRef.current || deployingRef.current) return;
		const currentService = servicesRef.current.find(
			(service) => service.id === serviceId,
		);
		if (!currentService) return;
		discardingRef.current = true;
		setDeployError(undefined);
		setPendingDiscard(snapshotApplyingChanges(currentService));
		setDiscardingChangeId(`service:${serviceId}`);
		if (
			!servicesRef.current.some(
				(service) => service.id !== serviceId && hasUnappliedChanges(service),
			)
		) {
			setShowChangeDetails(false);
		}
		const basisRevision = revisionRef.current;
		const draftVolume = currentService.spec?.runtime?.volume?.volumeName;
		const orphaned = stagedVolumesOnlyMountedBy(serviceId, draftVolume);
		try {
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
				handleVolumesDiscarded(orphaned);
				if (!hasOtherUndeployedServices) setShowChangeDetails(false);
				return;
			}
			const service = await doDiscardServiceChanges({
				data: { serviceId, discardAll: true },
			});
			mergeServiceRecord(service, basisRevision);
			if (service.spec?.runtime?.volume?.volumeName !== draftVolume) {
				handleVolumesDiscarded(orphaned);
			}
			if (!hasOtherUndeployedServices && !hasUnappliedChanges(service)) {
				setShowChangeDetails(false);
			}
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
			setShowChangeDetails(true);
		} finally {
			discardingRef.current = false;
			setPendingDiscard(undefined);
			setDiscardingChangeId(undefined);
		}
	};

	const handleDiscardChange = async (serviceId: string, changeId: string) => {
		if (discardingRef.current || deployingRef.current) return;
		const service = servicesRef.current.find(
			(service) => service.id === serviceId,
		);
		const change = service?.unappliedChanges.find(
			(change) => change.id === changeId,
		);
		if (!service || !change) return;
		discardingRef.current = true;
		setDeployError(undefined);
		setPendingDiscard({
			...snapshotApplyingChanges(service),
			count: 1,
			changeKeys: [applyingChangeKey(serviceId, change.id, change.newValue)],
		});
		setDiscardingChangeId(`${serviceId}:${changeId}`);
		const basisRevision = revisionRef.current;
		const draftVolume = service.spec?.runtime?.volume?.volumeName;
		try {
			const updated = await doDiscardServiceChanges({
				data: { serviceId, changeIds: [changeId] },
			});
			mergeServiceRecord(updated, basisRevision);
			if (
				draftVolume &&
				updated.spec?.runtime?.volume?.volumeName !== draftVolume
			) {
				handleVolumesDiscarded(
					stagedVolumesOnlyMountedBy(serviceId, draftVolume),
				);
			}
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
			setShowChangeDetails(true);
		} finally {
			discardingRef.current = false;
			setPendingDiscard(undefined);
			setDiscardingChangeId(undefined);
		}
	};

	const handleDiscardVolume = async (volumeId: string) => {
		if (discardingRef.current || deployingRef.current) return;
		discardingRef.current = true;
		setDeployError(undefined);
		setDiscardingChangeId(`volume:${volumeId}`);
		try {
			await doDeleteResource({ data: { kind: "volume", id: volumeId } });
			handleVolumesDiscarded([volumeId]);
		} catch (error) {
			setDeployError(`Discard failed: ${formatError(error)}`);
			setShowChangeDetails(true);
		} finally {
			discardingRef.current = false;
			setDiscardingChangeId(undefined);
		}
	};

	return {
		stagedVolumes,
		handleDiscardVolume,
		pendingCreationCount,
		discardingChangeCount,
		visibleServices,
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
