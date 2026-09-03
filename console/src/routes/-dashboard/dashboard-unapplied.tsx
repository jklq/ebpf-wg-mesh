import { Loader2, Trash2, UploadCloud, X } from "lucide-react";

import { cn } from "#/lib/cn";
import type {
	DashboardServiceRecord,
	DashboardUnappliedChangeAction,
} from "#/lib/dashboard/core/types.server";
import { btnPrimary, btnSecondary, iconBtn, modalCard } from "#/lib/ui-classes";

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
			<div className={cn(modalCard, "max-w-[820px]")}>
				<div className="flex items-center justify-between gap-3 border-b border-line p-4">
					<h2 className="m-0 font-display text-[26px] font-medium tracking-[-0.03em] text-ink">
						{totalChanges} changes to apply
					</h2>
					<button
						type="button"
						className={iconBtn}
						aria-label="Close"
						onClick={onClose}
					>
						<X size={16} />
					</button>
				</div>
				<div className="flex flex-col gap-3.5 p-4">
					{services.map((service) => (
						<div
							className="border border-line bg-[rgba(255,255,255,0.02)]"
							key={service.id}
						>
							<div className="grid grid-cols-[1fr_auto] items-center gap-3 border-b border-line px-3 py-2.5">
								<div>
									<strong className="block text-[13px] text-ink">
										{service.name}
									</strong>
									<span className="text-[11px] text-muted">
										{sectionSummary(service.unappliedChanges ?? [])}
									</span>
								</div>
								<button
									type="button"
									className={btnSecondary}
									onClick={() => onDiscardService(service.id)}
									disabled={discardingChangeId === `service:${service.id}`}
								>
									<Trash2 size={13} />
									Discard service changes
								</button>
							</div>
							{(service.unappliedChanges ?? []).map((change) => (
								<div
									className="grid grid-cols-[72px_minmax(120px,1fr)_minmax(220px,1.4fr)_32px] items-center gap-3 border-b border-line px-3 py-2.5 last:border-b-0 max-sm:grid-cols-[1fr_32px]"
									key={change.id}
								>
									<span
										className={cn(
											"change-action text-[10px] font-bold tracking-[0.08em] uppercase max-sm:col-span-full",
											unappliedChangeActionLabel(change.action),
											change.action === "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD"
												? "text-healthy"
												: change.action ===
														"SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE"
													? "text-failed"
													: change.action ===
															"SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE"
														? "text-building"
														: "text-muted",
										)}
									>
										{unappliedChangeActionLabel(change.action)}
									</span>
									<div>
										<strong className="block text-[13px] text-ink">
											{change.field}
										</strong>
										<span className="text-[11px] text-muted">
											{change.section}
										</span>
									</div>
									<div className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)] gap-2 max-sm:col-span-full">
										{change.currentValue && (
											<div className="flex min-w-0 flex-col gap-1">
												<span className="font-condensed text-[9px] font-bold tracking-[0.08em] text-dim uppercase">
													Current
												</span>
												<code className="min-w-0 overflow-hidden border border-line bg-[rgba(0,0,0,0.2)] px-[7px] py-[5px] font-mono text-[11px] text-ellipsis whitespace-nowrap text-muted">
													{change.currentValue}
												</code>
											</div>
										)}
										{!change.currentValue && (
											<div className="block" aria-hidden="true" />
										)}
										<div className="flex min-w-0 flex-col gap-1">
											<span className="font-condensed text-[9px] font-bold tracking-[0.08em] text-dim uppercase">
												New
											</span>
											<code className="min-w-0 overflow-hidden border border-line bg-[rgba(0,0,0,0.2)] px-[7px] py-[5px] font-mono text-[11px] text-ellipsis whitespace-nowrap text-muted">
												{change.newValue || "empty"}
											</code>
										</div>
									</div>
									<button
										type="button"
										className={cn(iconBtn, "self-end")}
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
				<div className="flex items-center justify-between gap-3 border-t border-line p-4 text-xs text-muted">
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
						<span className="min-w-0 overflow-hidden font-mono text-[11px] text-ellipsis whitespace-nowrap text-failed">
							{deployError}
						</span>
					)}
					<button
						type="button"
						className={btnPrimary}
						onClick={onDeploy}
						disabled={deploying || deployableChanges === 0}
					>
						{deploying ? (
							<Loader2 size={13} className="animate-spin" />
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

export function unappliedChangeActionLabel(
	action: DashboardUnappliedChangeAction,
): string {
	switch (action) {
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD":
			return "add";
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE":
			return "update";
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE":
			return "remove";
		case "SERVICE_UNAPPLIED_CHANGE_ACTION_UNSPECIFIED":
			return "unspecified";
	}
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
