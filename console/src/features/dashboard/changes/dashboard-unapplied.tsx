import * as stylex from "@stylexjs/stylex";
import { Button } from "#/components/ui/button";
import { dialogStyles } from "#/components/ui/dialog";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { Loader2, Trash2, UploadCloud, X } from "lucide-react";
import { Dialog } from "#/components/ui/dialog";
import { formatBytes } from "#/lib/bytes";
import type {
	DashboardServiceRecord,
	DashboardUnappliedChangeAction,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";

export type ApplyingServiceChanges = {
	serviceId: string;
	serviceName: string;
	changeKeys: Array<string>;
	count: number;
	specRevision?: string;
};

export function UnappliedChangesDialog({
	services,
	stagedVolumes = [],
	totalChanges,
	applyingChanges,
	deployableChanges,
	affectedServices,
	deploying,
	creationPending,
	deployError,
	discardingChangeId,
	onClose,
	onDeploy,
	onDiscardChange,
	onDiscardService,
	onDiscardVolume,
}: {
	services: Array<DashboardServiceRecord>;
	stagedVolumes?: Array<DashboardVolume>;
	totalChanges: number;
	applyingChanges: number;
	deployableChanges: number;
	affectedServices: number;
	deploying: boolean;
	creationPending: boolean;
	deployError?: string;
	discardingChangeId?: string;
	onClose: () => void;
	onDeploy: () => void;
	onDiscardChange: (serviceId: string, changeId: string) => void;
	onDiscardService: (serviceId: string) => void;
	onDiscardVolume?: (volumeId: string) => void;
}) {
	return (
		<Dialog label="Unapplied changes" onClose={onClose}>
			<div {...stylex.props([dialogStyles.card, styles.card])}>
				<div {...stylex.props(styles.header)}>
					<h2 {...stylex.props(styles.title)}>
						{totalChanges} changes to apply
					</h2>
					<Button
						type="button"
						variant="icon"
						aria-label="Close"
						onClick={onClose}
					>
						<X size={16} />
					</Button>
				</div>
				<div {...stylex.props(styles.serviceList)}>
					{services.map((service) => (
						<div {...stylex.props(styles.serviceGroup)} key={service.id}>
							<div {...stylex.props(styles.serviceHeader)}>
								<div>
									<strong {...stylex.props(styles.serviceName)}>
										{service.name}
									</strong>
									<span {...stylex.props(styles.serviceSummary)}>
										{sectionSummary(service.unappliedChanges ?? [])}
									</span>
								</div>
								<Button
									type="button"
									variant="secondary"
									onClick={() => onDiscardService(service.id)}
									disabled={deploying || Boolean(discardingChangeId)}
								>
									<Trash2 size={13} />
									Discard service changes
								</Button>
							</div>
							{(service.unappliedChanges ?? []).map((change) => (
								<div {...stylex.props(styles.changeRow)} key={change.id}>
									<span
										{...stylex.props([
											styles.changeAction,
											change.action === "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD"
												? styles.addedAction
												: change.action ===
														"SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE"
													? styles.removedAction
													: change.action ===
															"SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE"
														? styles.updatedAction
														: styles.defaultAction,
										])}
									>
										{unappliedChangeActionLabel(change.action)}
									</span>
									<div>
										<strong {...stylex.props(styles.serviceName)}>
											{change.field}
										</strong>
										<span {...stylex.props(styles.serviceSummary)}>
											{change.section}
										</span>
									</div>
									<div {...stylex.props(styles.valueComparison)}>
										{change.currentValue && (
											<div {...stylex.props(styles.valueColumn)}>
												<span {...stylex.props(styles.valueLabel)}>
													Current
												</span>
												<code {...stylex.props(styles.value)}>
													{change.currentValue}
												</code>
											</div>
										)}
										{!change.currentValue && (
											<div
												{...stylex.props(styles.valuePlaceholder)}
												aria-hidden="true"
											/>
										)}
										<div {...stylex.props(styles.valueColumn)}>
											<span {...stylex.props(styles.valueLabel)}>New</span>
											<code {...stylex.props(styles.value)}>
												{change.newValue || "empty"}
											</code>
										</div>
									</div>
									<Button
										type="button"
										variant="icon"
										styles={[styles.discardButton]}
										aria-label={`Discard ${change.field}`}
										onClick={() => onDiscardChange(service.id, change.id)}
										disabled={deploying || Boolean(discardingChangeId)}
									>
										<Trash2 size={14} />
									</Button>
								</div>
							))}
						</div>
					))}
					{stagedVolumes.length > 0 && (
						<div {...stylex.props(styles.serviceGroup)}>
							<div {...stylex.props(styles.serviceHeader)}>
								<strong {...stylex.props(styles.serviceName)}>
									New volumes
								</strong>
							</div>
							{stagedVolumes.map((volume) => {
								const owner = services.find(
									(service) =>
										service.spec?.runtime?.volume?.volumeName === volume.name,
								);
								return (
									<div {...stylex.props(styles.changeRow)} key={volume.id}>
										<span
											{...stylex.props([
												styles.changeAction,
												styles.addedAction,
											])}
										>
											add
										</span>
										<div>
											<strong {...stylex.props(styles.serviceName)}>
												{volume.name}
											</strong>
											<span {...stylex.props(styles.serviceSummary)}>
												{owner ? `Mounted by ${owner.name}` : "Not mounted"}
											</span>
										</div>
										<code {...stylex.props(styles.value)}>
											{formatBytes(volume.sizeBytes)}
										</code>
										<Button
											type="button"
											variant="icon"
											styles={[styles.discardButton]}
											aria-label={`Discard volume ${volume.name}`}
											title={
												owner
													? `Discarded with ${owner.name}'s changes`
													: undefined
											}
											onClick={() => onDiscardVolume?.(volume.id)}
											disabled={
												Boolean(owner) ||
												deploying ||
												Boolean(discardingChangeId)
											}
										>
											<Trash2 size={14} />
										</Button>
									</div>
								);
							})}
						</div>
					)}
				</div>
				<div {...stylex.props(styles.footer)}>
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
						<span {...stylex.props(styles.errorMessage)}>{deployError}</span>
					)}
					<Button
						type="button"
						variant="primary"
						onClick={onDeploy}
						disabled={
							deploying ||
							creationPending ||
							Boolean(discardingChangeId) ||
							deployableChanges === 0
						}
					>
						{deploying ? (
							<Loader2 size={13} {...stylex.props(styles.spinner)} />
						) : (
							<UploadCloud size={13} />
						)}
						{deployActionLabel({
							deploying,
						})}
					</Button>
				</div>
			</div>
		</Dialog>
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
		changeKeys:
			changes.length > 0
				? changes.map((change) =>
						applyingChangeKey(service.id, change.id, change.newValue),
					)
				: [
						applyingChangeKey(
							service.id,
							`service:${service.id}:${service.specRevision}`,
						),
					],
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
			!applyingChangeKeys.has(
				applyingChangeKey(service.id, change.id, change.newValue),
			),
	).length;
}

export function applyingChangeKey(
	serviceId: string,
	changeId: string,
	newValue?: string,
): string {
	return JSON.stringify([serviceId, changeId, newValue]);
}

export function dirtyPromptTitle({
	discarding = 0,
	creating = 0,
	applying,
	deployError,
	deploying = false,
}: {
	discarding?: number;
	creating?: number;
	applying: number;
	deployError?: string;
	deploying?: boolean;
}): string {
	if (deployError) return "Changes failed";
	if (discarding > 0) return "Discarding changes";
	if (deploying || applying > 0) return "Deploying changes";
	if (creating > 0) return "Creating services";
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
	discarding = 0,
	creating = 0,
	applying,
	deployable,
	saving = false,
	total,
}: {
	discarding?: number;
	creating?: number;
	applying: number;
	deployable: number;
	saving?: boolean;
	total: number;
}): string {
	if (discarding > 0)
		return `Discarding ${discarding} ${pluralizeChange(discarding)}${deployable > 0 ? `, ${deployable} ready` : ""}`;
	if (creating > 0) {
		return `Creating ${creating} ${creating === 1 ? "service" : "services"}${deployable > 0 ? `, ${deployable} changes ready` : ""}`;
	}
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

const styles = stylex.create({
	card: { maxWidth: "820px" },
	header: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.md,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		padding: space.lg,
	},
	title: {
		margin: "0rem",
		fontFamily: fonts.display,
		fontSize: "26px",
		fontWeight: "500",
		letterSpacing: "-0.03em",
		color: colors.ink,
	},
	serviceList: {
		display: "flex",
		flexDirection: "column",
		gap: "0.875rem",
		padding: space.lg,
	},
	serviceGroup: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "rgba(255,255,255,0.02)",
	},
	serviceHeader: {
		display: "grid",
		gridTemplateColumns: "1fr auto",
		alignItems: "center",
		gap: space.md,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
	},
	serviceName: { display: "block", fontSize: "13px", color: colors.ink },
	serviceSummary: { fontSize: "11px", color: colors.muted },
	changeRow: {
		display: "grid",
		gridTemplateColumns: {
			default: "72px minmax(120px,1fr) minmax(220px,1.4fr) 32px",
			"@media (width < 40rem)": "1fr 32px",
		},
		alignItems: "center",
		gap: space.md,
		borderBottomStyle: { default: "solid", ":last-child": "solid" },
		borderBottomWidth: { default: "1px", ":last-child": "0px" },
		borderColor: colors.line,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
	},
	changeAction: {
		fontSize: "10px",
		fontWeight: "700",
		letterSpacing: "0.08em",
		textTransform: "uppercase",
		gridColumn: { default: null, "@media (width < 40rem)": "1 / -1" },
	},
	addedAction: { color: colors.healthy },
	removedAction: { color: colors.failed },
	updatedAction: { color: colors.building },
	defaultAction: { color: colors.muted },
	valueComparison: {
		display: "grid",
		gridTemplateColumns: "minmax(0,1fr) minmax(0,1fr)",
		gap: space.sm,
		gridColumn: { default: null, "@media (width < 40rem)": "1 / -1" },
	},
	valueColumn: {
		display: "flex",
		minWidth: "0rem",
		flexDirection: "column",
		gap: space.xs,
	},
	valueLabel: {
		fontFamily: fonts.condensed,
		fontSize: "9px",
		fontWeight: "700",
		letterSpacing: "0.08em",
		color: colors.dim,
		textTransform: "uppercase",
	},
	value: {
		minWidth: "0rem",
		overflow: "hidden",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "rgba(0,0,0,0.2)",
		paddingInline: "7px",
		paddingBlock: "5px",
		fontFamily: fonts.mono,
		fontSize: "11px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.muted,
	},
	valuePlaceholder: { display: "block" },
	discardButton: { alignSelf: "flex-end" },
	footer: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.md,
		borderTopStyle: "solid",
		borderTopWidth: "1px",
		borderColor: colors.line,
		padding: space.lg,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	errorMessage: {
		minWidth: "0rem",
		overflow: "hidden",
		fontFamily: fonts.mono,
		fontSize: "11px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.failed,
	},
	spinner: { animation: `${spin} 1s linear infinite` },
});
