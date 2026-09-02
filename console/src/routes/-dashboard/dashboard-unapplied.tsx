import { Loader2, Trash2, UploadCloud, X } from "lucide-react";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

import { ModalOverlay } from "./ui";

export type ApplyingServiceChanges = {
	serviceId: string;
	serviceName: string;
	changeIds: Array<string>;
	count: number;
	specRevision?: number;
};

export function UnappliedChangesDialog({
	services,
	totalChanges,
	applyingChanges,
	deployableChanges,
	affectedServices,
	deploying,
	deployError,
	discardingChangeId,
	onClose,
	onDeploy,
	onDiscardChange,
	onDiscardService,
}: {
	services: Array<DashboardServiceRecord>;
	totalChanges: number;
	applyingChanges: number;
	deployableChanges: number;
	affectedServices: number;
	deploying: boolean;
	deployError?: string;
	discardingChangeId?: string;
	onClose: () => void;
	onDeploy: () => void;
	onDiscardChange: (serviceId: string, changeId: string) => void;
	onDiscardService: (serviceId: string) => void;
}) {
	return (
		<ModalOverlay onClose={onClose}>
			<div className="modal-card unapplied-dialog">
				<div className="unapplied-dialog-header">
					<h2>{totalChanges} changes to apply</h2>
					<button
						type="button"
						className="icon-btn"
						aria-label="Close"
						onClick={onClose}
					>
						<X size={16} />
					</button>
				</div>
				<div className="unapplied-service-list">
					{services.map((service) => (
						<div className="unapplied-service-group" key={service.id}>
							<div className="unapplied-service-heading">
								<div>
									<strong>{service.name}</strong>
									<span>{sectionSummary(service.unappliedChanges ?? [])}</span>
								</div>
								<button
									type="button"
									className="btn-secondary"
									onClick={() => onDiscardService(service.id)}
									disabled={discardingChangeId === `service:${service.id}`}
								>
									<Trash2 size={13} />
									Discard service changes
								</button>
							</div>
							{(service.unappliedChanges ?? []).map((change) => (
								<div className="unapplied-change-row" key={change.id}>
									<span className={`change-action ${change.action}`}>
										{change.action}
									</span>
									<div className="change-field">
										<strong>{change.field}</strong>
										<span>{change.section}</span>
									</div>
									<div className="change-values">
										{change.currentValue && (
											<div>
												<span>Current</span>
												<code>{change.currentValue}</code>
											</div>
										)}
										{!change.currentValue && (
											<div className="change-value-spacer" aria-hidden="true" />
										)}
										<div>
											<span>New</span>
											<code>{change.newValue || "empty"}</code>
										</div>
									</div>
									<button
										type="button"
										className="icon-btn"
										aria-label={`Discard ${change.field}`}
										onClick={() => onDiscardChange(service.id, change.id)}
										disabled={
											discardingChangeId === `${service.id}:${change.id}`
										}
									>
										<Trash2 size={14} />
									</button>
								</div>
							))}
						</div>
					))}
				</div>
				<div className="unapplied-dialog-footer">
					<span>
						{dirtyPromptDetail({
							applying: applyingChanges,
							deployable: deployableChanges,
							total: totalChanges,
						})}{" "}
						across {affectedServices}{" "}
						{affectedServices === 1 ? "service" : "services"}
					</span>
					{deployError && (
						<span className="dirty-workspace-error">{deployError}</span>
					)}
					<button
						type="button"
						className="btn-primary"
						onClick={onDeploy}
						disabled={deploying || deployableChanges === 0}
					>
						{deploying ? (
							<Loader2
								size={13}
								style={{ animation: "spin 1s linear infinite" }}
							/>
						) : (
							<UploadCloud size={13} />
						)}
						{deployActionLabel({
							deploying,
						})}
					</button>
				</div>
			</div>
		</ModalOverlay>
	);
}

export function unappliedChangeCount(service: DashboardServiceRecord): number {
	return service.unappliedChangeCount ?? (service.pendingChanges ? 1 : 0);
}

export function hasUnappliedChanges(service: DashboardServiceRecord): boolean {
	return unappliedChangeCount(service) > 0;
}

export function snapshotApplyingChanges(
	service: DashboardServiceRecord,
): ApplyingServiceChanges {
	const changes = service.unappliedChanges ?? [];
	const count = unappliedChangeCount(service);
	return {
		serviceId: service.id,
		serviceName: service.name,
		changeIds:
			changes.length > 0
				? changes.map((change) => change.id)
				: [`service:${service.id}:${service.specRevision ?? "current"}`],
		count,
		specRevision: service.specRevision,
	};
}

export function mergeApplyingServices(
	current: Array<ApplyingServiceChanges>,
	next: Array<ApplyingServiceChanges>,
): Array<ApplyingServiceChanges> {
	const services = new Map<string, ApplyingServiceChanges>();
	for (const service of current) {
		services.set(service.serviceId, service);
	}
	for (const service of next) {
		services.set(service.serviceId, service);
	}
	return Array.from(services.values());
}

export function queuedChangeCount(
	service: DashboardServiceRecord,
	applyingChangeKeys: Set<string>,
): number {
	const changes = service.unappliedChanges ?? [];
	if (changes.length === 0) {
		return applyingChangeKeys.has(
			applyingChangeKey(
				service.id,
				`service:${service.id}:${service.specRevision ?? "current"}`,
			),
		)
			? 0
			: unappliedChangeCount(service);
	}
	return changes.filter(
		(change) =>
			!applyingChangeKeys.has(applyingChangeKey(service.id, change.id)),
	).length;
}

export function applyingChangeKey(serviceId: string, changeId: string): string {
	return `${serviceId}:${changeId}`;
}

export function dirtyPromptTitle({
	applying,
	deployError,
	deploying = false,
}: {
	applying: number;
	deployError?: string;
	deploying?: boolean;
}): string {
	if (deployError) return "Deploy failed";
	if (deploying || applying > 0) return "Deploying changes";
	return "Undeployed changes";
}

export function deployActionLabel({
	deploying,
}: {
	deploying: boolean;
}): string {
	if (deploying) return "Deploying…";
	return "Deploy changes";
}

export function dirtyPromptDetail({
	applying,
	deployable,
	saving = false,
	total,
}: {
	applying: number;
	deployable: number;
	saving?: boolean;
	total: number;
}): string {
	if (applying > 0 && deployable > 0) {
		return `Applying ${applying} ${pluralizeChange(applying)}, ${deployable} ready`;
	}
	if (applying > 0) {
		return `Applying ${applying} ${pluralizeChange(applying)}`;
	}
	if (saving) return "Updating undeployed changes";
	return `Apply ${total} ${pluralizeChange(total)}`;
}

function pluralizeChange(count: number): string {
	return count === 1 ? "change" : "changes";
}

function sectionSummary(
	changes: NonNullable<DashboardServiceRecord["unappliedChanges"]>,
): string {
	const counts = new Map<string, number>();
	for (const change of changes) {
		counts.set(change.section, (counts.get(change.section) ?? 0) + 1);
	}
	return Array.from(counts.entries())
		.map(([section, count]) => `${section} ${count}`)
		.join(" · ");
}
