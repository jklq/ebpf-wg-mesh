import { useEffect, useRef, useState } from "react";

import type {
	DashboardDomainBinding,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { DomainPanelView, recommendedTargetPort } from "./panel-domains-list";
import {
	doCreateDomainBinding,
	doDeleteDomainBinding,
	doGenerateDomainBinding,
	doUpdateDomainBinding,
	fetchDomainBindings,
} from "./server-fns";
import { formatError } from "./service-utils";
import { usePolling } from "./use-polling";

export function PanelDomains({
	service,
	state,
	status = null,
}: {
	service: DashboardServiceRecord;
	state: DashboardHomeState;
	status?: DashboardServiceStatus | null;
}) {
	const recommendedPort = recommendedTargetPort(service, status);
	const [bindings, setBindings] = useState<DashboardDomainBinding[]>([]);
	const [loadingBindings, setLoadingBindings] = useState(true);
	const [hostname, setHostname] = useState("");
	const [targetPort, setTargetPort] = useState("");
	const [domainFlow, setDomainFlow] = useState<"generate" | "custom" | null>(
		null,
	);
	const [generating, setGenerating] = useState(false);
	// A domain that has been requested but is not confirmed live yet. It is shown
	// in the list straight away with a spinner so the flow never blocks on a modal.
	const [pendingDomain, setPendingDomain] = useState<{
		hostname?: string;
	} | null>(null);
	const [publishing, setPublishing] = useState(false);
	const [error, setError] = useState<string>();
	const [success, setSuccess] = useState<string>();

	const [editingBinding, setEditingBinding] =
		useState<DashboardDomainBinding | null>(null);
	const [editPort, setEditPort] = useState("");
	const [editSaving, setEditSaving] = useState(false);
	const [editError, setEditError] = useState<string>();

	const [deletingHostname, setDeletingHostname] = useState<string | null>(null);
	const [deleteConfirm, setDeleteConfirm] = useState<string | null>(null);

	const hostnameId = `domain-hostname-${service.id}`;
	const targetPortId = `domain-target-port-${service.id}`;
	const targetPortRef = useRef<HTMLInputElement>(null);
	const hostnameRef = useRef<HTMLInputElement>(null);
	const primaryDomainActionRef = useRef<HTMLButtonElement>(null);
	const editPortRef = useRef<HTMLInputElement>(null);
	const removeDomainRef = useRef<HTMLButtonElement>(null);
	const platformBinding = bindings.find((binding) => binding.platformGenerated);
	const customBindings = bindings.filter(
		(binding) => !binding.platformGenerated,
	);
	const visibleBindings = customBindings.length > 0 ? customBindings : bindings;
	const internalHostname =
		service.internalHostname ?? `${service.name}.mesh.internal`;
	const internalShortName = internalHostname.replace(/\.mesh\.internal$/, "");

	useEffect(() => {
		setLoadingBindings(true);
		setTargetPort("");
		setHostname("");
		setDomainFlow(null);
		fetchDomainBindings({
			data: { serviceId: service.id },
		})
			.then(setBindings)
			.catch(() => setBindings([]))
			.finally(() => setLoadingBindings(false));
	}, [service.id]);

	useEffect(() => {
		if (!domainFlow) return;
		if (domainFlow === "custom" && platformBinding) {
			hostnameRef.current?.focus();
			return;
		}
		(targetPortRef.current ?? primaryDomainActionRef.current)?.focus();
	}, [domainFlow, platformBinding]);

	useEffect(() => {
		if (editingBinding) editPortRef.current?.focus();
	}, [editingBinding]);

	useEffect(() => {
		if (deleteConfirm) removeDomainRef.current?.focus();
	}, [deleteConfirm]);

	const needsOwnershipPoll = bindings.some(
		(binding) =>
			!binding.platformGenerated &&
			binding.ownershipState !== "DOMAIN_OWNERSHIP_STATE_VERIFIED",
	);

	usePolling(
		async () => {
			try {
				setBindings(
					await fetchDomainBindings({ data: { serviceId: service.id } }),
				);
			} catch {
				// Keep the last known bindings during a transient refresh failure.
			}
		},
		{ enabled: needsOwnershipPoll, intervalMs: 5000 },
	);

	const openDomainFlow = (flow: "generate" | "custom") => {
		setDomainFlow(flow);
		setTargetPort(platformBinding ? String(platformBinding.targetPort) : "");
		setError(undefined);
		setSuccess(undefined);
	};

	const handleGenerate = async ({
		keepFlowOpen,
	}: {
		keepFlowOpen: boolean;
	}) => {
		setError(undefined);
		setSuccess(undefined);
		setGenerating(true);
		if (!keepFlowOpen) {
			// Close immediately — the pending row in the list carries the progress.
			setDomainFlow(null);
			setPendingDomain({});
		}
		try {
			const binding = await doGenerateDomainBinding({
				data: {
					serviceId: service.id,
					targetPort: targetPort.trim() || String(recommendedPort),
				},
			});
			setBindings((previous) => [
				...previous.filter((item) => !item.platformGenerated),
				binding,
			]);
			if (!keepFlowOpen) {
				// Keep the spinner on the row until the platform lists the binding —
				// that is when routing for it is actually in place.
				setPendingDomain({ hostname: binding.hostname });
				await fetchDomainBindings({ data: { serviceId: service.id } })
					.then(setBindings)
					.catch(() => undefined);
				setPendingDomain(null);
			}
			setSuccess(`${binding.hostname} is ready.`);
		} catch (e) {
			setPendingDomain(null);
			setError(formatError(e));
		} finally {
			setGenerating(false);
		}
	};

	const handlePublish = async () => {
		setError(undefined);
		setSuccess(undefined);
		setPublishing(true);
		try {
			const binding = await doCreateDomainBinding({
				data: {
					serviceId: service.id,
					hostname: hostname.trim(),
					targetPort: String(platformBinding?.targetPort ?? recommendedPort),
				},
			});
			setBindings((prev) => [...prev, binding]);
			setHostname("");
			setSuccess(
				binding.ownershipState === "DOMAIN_OWNERSHIP_STATE_VERIFIED"
					? `${binding.hostname} is now live.`
					: `${binding.hostname} added. Waiting for DNS to point at the platform hostname.`,
			);
			setDomainFlow(null);
		} catch (e) {
			setError(formatError(e));
		} finally {
			setPublishing(false);
		}
	};

	const openEdit = (binding: DashboardDomainBinding) => {
		setEditingBinding(binding);
		setEditPort(String(binding.targetPort));
		setEditError(undefined);
	};

	const handleEditSave = async () => {
		if (!editingBinding || editSaving || editPort.trim() === "") return;
		setEditError(undefined);
		setEditSaving(true);
		try {
			const updated = await doUpdateDomainBinding({
				data: {
					serviceId: editingBinding.serviceId,
					hostname: editingBinding.hostname,
					targetPort: editPort,
				},
			});
			setBindings((prev) =>
				prev.map((b) => (b.hostname === updated.hostname ? updated : b)),
			);
			setEditingBinding(null);
		} catch (e) {
			setEditError(formatError(e));
		} finally {
			setEditSaving(false);
		}
	};

	const handleDelete = async (h: string) => {
		setDeletingHostname(h);
		try {
			await doDeleteDomainBinding({
				data: { hostname: h },
			});
			setBindings((prev) => {
				const remaining = prev.filter((item) => item.hostname !== h);
				if (remaining.some((item) => !item.platformGenerated)) {
					return remaining;
				}
				return remaining.filter((item) => !item.platformGenerated);
			});
			await fetchDomainBindings({ data: { serviceId: service.id } })
				.then(setBindings)
				.catch(() => undefined);
		} catch (e) {
			setError(formatError(e));
		} finally {
			setDeletingHostname(null);
			setDeleteConfirm(null);
		}
	};

	return (
		<DomainPanelView
			state={state}
			domainFlow={domainFlow}
			platformBinding={platformBinding}
			targetPortId={targetPortId}
			targetPortRef={targetPortRef}
			targetPort={targetPort}
			setTargetPort={setTargetPort}
			setError={setError}
			recommendedPort={recommendedPort}
			hostnameId={hostnameId}
			hostnameRef={hostnameRef}
			hostname={hostname}
			setHostname={setHostname}
			error={error}
			success={success}
			primaryDomainActionRef={primaryDomainActionRef}
			generating={generating}
			publishing={publishing}
			setDomainFlow={setDomainFlow}
			handlePublish={handlePublish}
			handleGenerate={handleGenerate}
			editingBinding={editingBinding}
			setEditingBinding={setEditingBinding}
			handleEditSave={handleEditSave}
			editPortRef={editPortRef}
			editPort={editPort}
			setEditPort={setEditPort}
			setEditError={setEditError}
			editError={editError}
			editSaving={editSaving}
			deleteConfirm={deleteConfirm}
			setDeleteConfirm={setDeleteConfirm}
			deletingHostname={deletingHostname}
			handleDelete={handleDelete}
			removeDomainRef={removeDomainRef}
			internalHostname={internalHostname}
			internalShortName={internalShortName}
			loadingBindings={loadingBindings}
			visibleBindings={visibleBindings}
			pendingDomain={pendingDomain}
			openEdit={openEdit}
			openDomainFlow={openDomainFlow}
		/>
	);
}
