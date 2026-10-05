import * as stylex from "@stylexjs/stylex";
import { Button, buttonStyles } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";
import { DomainDialogs } from "#/features/dashboard/service-panel/domains/domain-dialogs";
import {
	type DomainBindingPhase,
	domainBindingPhase,
	formatCertificateMessage,
	formatOwnershipMessage,
	servesHTTPS,
} from "#/features/dashboard/service-panel/domains/domain-model";
import { useServiceDomains } from "#/features/dashboard/service-panel/domains/use-service-domains";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import {
	CircleAlert,
	CircleCheck,
	Globe,
	Loader2,
	Lock,
	Pencil,
	Trash2,
} from "lucide-react";
import { PanelSection } from "#/components/ui/section";

import { buildServiceURL } from "#/features/dashboard/shared/service-utils";
import type {
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

export function PanelDomains({
	service,
	state,
	status = null,
}: {
	service: DashboardServiceRecord;
	state: DashboardHomeState;
	status?: DashboardServiceStatus | null;
}) {
	const model = useServiceDomains({ service, status });
	const { platformBinding, domainFlow, error, success, openDomainFlow } =
		model.creation;
	const { openEdit } = model.editing;
	const { setDeleteConfirm } = model.removal;
	const {
		internalHostname,
		internalShortName,
		loadingBindings,
		visibleBindings,
		pendingDomain,
	} = model.network;
	return (
		<div {...stylex.props(styles.panel)}>
			<DomainDialogs
				creation={model.creation}
				editing={model.editing}
				removal={model.removal}
			/>

			<PanelSection
				title="Private mesh"
				lede="Reach this service from anything else in the current environment."
			>
				<div {...stylex.props(styles.privateNetwork)}>
					<CircleCheck
						size={20}
						aria-hidden="true"
						{...stylex.props(styles.readyIcon)}
					/>
					<div {...stylex.props(styles.privateNetworkCopy)}>
						<div {...stylex.props(styles.privateHostname)}>
							<span>{internalHostname}</span>
							<span {...stylex.props(styles.protocolBadge)}>IPv6</span>
						</div>
						<p {...stylex.props(styles.privateDescription)}>
							Ready to talk privately · You can also simply call me{" "}
							<code {...stylex.props(styles.shortHostname)}>
								{internalShortName}
							</code>
						</p>
					</div>
				</div>
			</PanelSection>

			<PanelSection
				title="Public domains"
				lede="Hostnames that route into this service."
			>
				<div {...stylex.props(styles.publicBindings)}>
					{loadingBindings && (
						<div {...stylex.props(styles.bindingsLoading)}>
							<Loader2 size={13} {...stylex.props(styles.spinner)} />
							Loading…
						</div>
					)}
					{!loadingBindings &&
						visibleBindings.length === 0 &&
						!pendingDomain && (
							<div {...stylex.props(styles.bindingsEmpty)}>
								No public domains yet.
							</div>
						)}
					{pendingDomain && !pendingDomain.hostname && (
						<div {...stylex.props(styles.pendingBinding)}>
							<div {...stylex.props(styles.pendingBindingContent)}>
								<Loader2 size={12} {...stylex.props(styles.spinner)} />
								<span {...stylex.props(styles.pendingLabel)}>
									Generating domain…
								</span>
							</div>
						</div>
					)}
					{visibleBindings.map((binding) => {
						const pending = pendingDomain?.hostname === binding.hostname;
						const phase = domainBindingPhase(binding);
						const unverified = phase === "awaiting-dns";
						const waiting = pending || phase !== "live";
						const https = servesHTTPS(binding);
						const certificateMessage = formatCertificateMessage(binding);
						return (
							<div
								key={binding.hostname}
								{...stylex.props([
									styles.bindingCard,
									waiting ? styles.pendingBindingCard : styles.readyBindingCard,
								])}
							>
								<div {...stylex.props(styles.bindingHeader)}>
									<div {...stylex.props(styles.bindingSummary)}>
										<div {...stylex.props(styles.pendingBindingContent)}>
											{phase === "certificate-failed" ? (
												<CircleAlert
													size={12}
													{...stylex.props(styles.failedIcon)}
												/>
											) : waiting ? (
												<Loader2 size={12} {...stylex.props(styles.spinner)} />
											) : https ? (
												<Lock
													size={12}
													aria-label="HTTPS"
													{...stylex.props(styles.readyIcon)}
												/>
											) : (
												<Globe size={12} {...stylex.props(styles.readyIcon)} />
											)}
											<span {...stylex.props(styles.hostname)}>
												{binding.hostname}
												<span {...stylex.props(styles.targetPort)}>
													{" "}
													-&gt; :{binding.targetPort}
												</span>
											</span>
										</div>
										<div {...stylex.props(styles.bindingActions)}>
											{!pending && (
												<span
													{...stylex.props([
														styles.ownershipBadge,
														phaseBadgeStyles[phase],
													])}
												>
													{phaseLabels[phase]}
												</span>
											)}
											<a
												href={buildServiceURL(state, binding.hostname, {
													https,
												})}
												target="_blank"
												rel="noreferrer"
												{...stylex.props([
													buttonStyles.base,
													buttonStyles.ghost,
													styles.openLink,
												])}
											>
												Open ↗
											</a>
											<Button
												type="button"
												variant="ghost"
												styles={[styles.closeButton]}
												onClick={() => openEdit(binding)}
												title="Edit"
											>
												<Pencil size={12} />
											</Button>
											<Button
												type="button"
												variant="ghost"
												styles={[styles.removeButton]}
												onClick={() => setDeleteConfirm(binding.hostname)}
												title="Remove"
											>
												<Trash2 size={12} />
											</Button>
										</div>
									</div>
									{unverified && (
										<div {...stylex.props(styles.ownershipDetails)}>
											<p {...stylex.props(styles.ownershipMessage)}>
												<CircleAlert size={12} aria-hidden="true" />
												{formatOwnershipMessage(
													binding.ownershipMessage,
													platformBinding?.hostname,
												)}
											</p>
											{platformBinding && (
												<p {...stylex.props(styles.cnameRecord)}>
													{binding.hostname} CNAME {platformBinding.hostname}
												</p>
											)}
										</div>
									)}
									{!unverified && certificateMessage && (
										<p {...stylex.props(styles.ownershipMessage)}>
											<CircleAlert size={12} aria-hidden="true" />
											{certificateMessage}
										</p>
									)}
								</div>
							</div>
						);
					})}
					{!domainFlow && error && (
						<p {...stylex.props(noticeStyles.error)}>{error}</p>
					)}
					{!domainFlow && success && (
						<p {...stylex.props(noticeStyles.success)}>{success}</p>
					)}
				</div>
				<div {...stylex.props(styles.createActions)}>
					<Button
						type="button"
						variant="primary"
						onClick={() => openDomainFlow("generate")}
					>
						Generate Domain
					</Button>
					<Button
						type="button"
						variant="secondary"
						onClick={() => openDomainFlow("custom")}
					>
						Custom Domain
					</Button>
				</div>
			</PanelSection>
		</div>
	);
}

const phaseLabels: Record<DomainBindingPhase, string> = {
	"awaiting-dns": "Waiting for CNAME",
	"issuing-certificate": "Issuing certificate",
	"certificate-failed": "Certificate failed",
	live: "Live",
};

const styles = stylex.create({
	panel: { display: "flex", flexDirection: "column", gap: "1.75rem" },
	closeButton: { paddingInline: "0.375rem", paddingBlock: space.xs },
	spinner: { animation: `${spin} 1s linear infinite` },
	privateNetwork: {
		display: "flex",
		alignItems: "center",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(109,190,130,0.28)",
		backgroundImage: `linear-gradient(180deg,rgba(109,190,130,0.08),transparent 55%),${colors.surfaceRaised}`,
		padding: "0.875rem",
	},
	readyIcon: { flexShrink: "0", color: colors.healthy },
	privateNetworkCopy: { minWidth: "0rem" },
	privateHostname: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: "7px",
		fontFamily: fonts.mono,
		fontSize: "13px",
		color: colors.ink,
	},
	protocolBadge: {
		backgroundColor: colors.accentDim,
		paddingInline: "5px",
		paddingBlock: "0.125rem",
		fontFamily: fonts.sans,
		fontSize: "10px",
		fontWeight: "600",
		color: colors.accent,
	},
	privateDescription: {
		marginTop: "5px",
		marginBottom: "0rem",
		fontSize: "11px",
		color: colors.muted,
	},
	shortHostname: {
		backgroundColor: colors.accentDim,
		paddingInline: space.xs,
		paddingBlock: "1px",
		fontFamily: fonts.mono,
		color: colors.accent,
	},
	publicBindings: { display: "flex", flexDirection: "column", gap: space.sm },
	bindingsLoading: {
		display: "flex",
		gap: "0.375rem",
		fontSize: "13px",
		color: colors.muted,
	},
	bindingsEmpty: {
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		paddingInline: space.lg,
		paddingBlock: "18px",
		fontSize: "13px",
		color: colors.muted,
	},
	pendingBinding: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.sm,
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		paddingInline: "0.875rem",
		paddingBlock: space.md,
		color: colors.muted,
	},
	pendingBindingContent: {
		display: "flex",
		alignItems: "center",
		gap: "0.375rem",
		overflow: "hidden",
	},
	pendingLabel: {
		fontFamily: fonts.mono,
		fontSize: "13px",
		color: colors.muted,
	},
	bindingCard: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		backgroundColor: colors.surfaceRaised,
		paddingInline: "0.875rem",
		paddingBlock: space.md,
	},
	pendingBindingCard: {
		borderStyle: "dashed",
		borderColor: colors.line,
		color: colors.muted,
	},
	readyBindingCard: { borderColor: colors.line },
	bindingHeader: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		flexDirection: "column",
		gap: space.sm,
	},
	bindingSummary: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.sm,
	},
	hostname: {
		overflow: "hidden",
		fontFamily: fonts.mono,
		fontSize: "13px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.ink,
	},
	targetPort: { color: colors.muted },
	bindingActions: {
		display: "flex",
		flexShrink: "0",
		alignItems: "center",
		gap: space.xs,
	},
	ownershipBadge: {
		fontSize: "10px",
		fontWeight: "600",
		letterSpacing: "0.04em",
		whiteSpace: "nowrap",
		textTransform: "uppercase",
	},
	unverifiedBadge: { color: colors.accent },
	liveBadge: { color: colors.healthy },
	failedBadge: { color: colors.failed },
	failedIcon: { flexShrink: "0", color: colors.failed },
	openLink: { fontSize: "11px" },
	removeButton: {
		paddingInline: "0.375rem",
		paddingBlock: space.xs,
		color: colors.failed,
	},
	ownershipDetails: {
		display: "flex",
		flexDirection: "column",
		gap: space.xs,
	},
	ownershipMessage: {
		margin: "0rem",
		display: "flex",
		alignItems: "flex-start",
		gap: "0.375rem",
		fontSize: "11px",
		lineHeight: "1.45",
		color: colors.muted,
	},
	cnameRecord: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		paddingInline: "0.625rem",
		paddingBlock: space.sm,
		fontFamily: fonts.mono,
		color: colors.ink,
	},
	createActions: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		columnGap: "0.875rem",
		rowGap: "0.625rem",
		paddingTop: "0.125rem",
	},
});

const phaseBadgeStyles: Record<DomainBindingPhase, stylex.StyleXStyles> = {
	"awaiting-dns": styles.unverifiedBadge,
	"issuing-certificate": styles.unverifiedBadge,
	"certificate-failed": styles.failedBadge,
	live: styles.liveBadge,
};
