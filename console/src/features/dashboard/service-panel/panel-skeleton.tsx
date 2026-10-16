import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { SkeletonChip } from "#/components/ui/skeleton";
import type { DashboardTab } from "#/features/dashboard/shared/types";
import { usagePanelLayout } from "#/features/usage/usage-panel-layout";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { colors, shape, space } from "#/styles/tokens.stylex";

function LoadingLabel({ label }: { label: string }) {
	return <output {...stylex.props(styles.hidden)}>{label}</output>;
}

function Chip({ width, height }: { width: number | string; height: number }) {
	return <SkeletonChip styles={styles.size(width, height)} />;
}

/** The placeholder for one service panel tab while its code or data loads. */
export function TabSkeleton({
	tab,
	service,
}: {
	tab: DashboardTab;
	service: DashboardServiceRecord;
}) {
	if (tab === "deployments") return <DeploymentsSkeleton />;
	return (
		<div {...stylex.props(tabContentStyles.content)}>
			{tab === "variables" && (
				<VariablesSkeleton
					rows={Object.keys(service.spec?.runtime?.env ?? {}).length}
				/>
			)}
			{tab === "domains" && <DomainsSkeleton />}
			{tab === "usage" && <UsagePanelSkeleton />}
			{tab === "settings" && <SettingsSkeleton />}
		</div>
	);
}

/** The scrolling, padded body every tab but deployments renders into. */
export const tabContentStyles = stylex.create({
	content: {
		display: "flex",
		height: "100%",
		flexDirection: "column",
		gap: space.lg,
		overflowY: "auto",
		paddingInline: "18px",
		paddingTop: "18px",
		paddingBottom: "1.75rem",
	},
});

/** Shaped like PanelSection: a heading, an optional lede, a rule, content. */
function SectionSkeleton({
	lede = false,
	children,
}: {
	lede?: boolean;
	children: ReactNode;
}) {
	return (
		<div>
			<div {...stylex.props(styles.sectionHeader)}>
				<span {...stylex.props(styles.sectionTitle)}>
					<Chip width={120} height={18} />
				</span>
				{lede && (
					<span {...stylex.props(styles.sectionLede)}>
						<Chip width="60%" height={11} />
					</span>
				)}
			</div>
			<div {...stylex.props(styles.sectionContent)}>{children}</div>
		</div>
	);
}

/** A label above a text input. */
function FieldSkeleton({ width = "100%" }: { width?: number | string }) {
	return (
		<div>
			<span {...stylex.props(styles.fieldLabel)}>
				<Chip width={72} height={9} />
			</span>
			<Chip width={width} height={34} />
		</div>
	);
}

function DeploymentsSkeleton() {
	return (
		<div {...stylex.props(styles.deployments)}>
			<LoadingLabel label="Loading deployments…" />
			<div aria-hidden="true" {...stylex.props(styles.deploymentsMetadata)}>
				<Chip width={180} height={12} />
				<Chip width={72} height={12} />
			</div>
			<DeploymentCardSkeleton labelled={false} />
		</div>
	);
}

/** Placeholder shaped like a live deployment card's header. */
export function DeploymentCardSkeleton({
	labelled = true,
}: {
	labelled?: boolean;
}) {
	return (
		<>
			{labelled && <LoadingLabel label="Loading deployments…" />}
			<div aria-hidden="true" {...stylex.props(styles.deploymentCard)}>
				<Chip width={64} height={20} />
				<SkeletonChip styles={styles.avatar} />
				<div {...stylex.props(styles.deploymentCopy)}>
					<Chip width="60%" height={15} />
					<Chip width="35%" height={10} />
				</div>
				<Chip width={96} height={32} />
				<Chip width={32} height={32} />
			</div>
		</>
	);
}

function VariablesSkeleton({ rows }: { rows: number }) {
	return (
		<>
			<LoadingLabel label="Loading variables…" />
			<div aria-hidden="true" {...stylex.props(styles.tab)}>
				<SectionSkeleton lede>
					<div {...stylex.props(styles.spread)}>
						<Chip width={124} height={32} />
						<Chip width={128} height={30} />
					</div>
					<div {...stylex.props(styles.variableList)}>
						{rows === 0 ? (
							<SkeletonChip styles={styles.variablesEmpty} />
						) : (
							Array.from({ length: rows }, (_, index) => (
								// biome-ignore lint/suspicious/noArrayIndexKey: static placeholders
								<div key={index} {...stylex.props(styles.variableRow)}>
									<FieldSkeleton />
									<FieldSkeleton />
									<Chip width={32} height={34} />
								</div>
							))
						)}
					</div>
				</SectionSkeleton>
			</div>
		</>
	);
}

/** Placeholder for one public domain binding. */
export function DomainBindingSkeleton() {
	return (
		<div aria-hidden="true" {...stylex.props(styles.bindingCard)}>
			<Chip width={12} height={12} />
			<Chip width="40%" height={12} />
			<span {...stylex.props(styles.spacer)} />
			<Chip width={44} height={18} />
			<Chip width={52} height={24} />
			<Chip width={24} height={24} />
			<Chip width={24} height={24} />
		</div>
	);
}

function DomainsSkeleton() {
	return (
		<>
			<LoadingLabel label="Loading domains…" />
			<div aria-hidden="true" {...stylex.props(styles.sections)}>
				<SectionSkeleton lede>
					<div {...stylex.props(styles.privateNetwork)}>
						<Chip width={20} height={20} />
						<div {...stylex.props(styles.stack)}>
							<Chip width={220} height={13} />
							<Chip width={260} height={10} />
						</div>
					</div>
				</SectionSkeleton>
				<SectionSkeleton lede>
					<DomainBindingSkeleton />
					<div {...stylex.props(styles.row)}>
						<Chip width={136} height={29} />
						<Chip width={128} height={29} />
					</div>
				</SectionSkeleton>
			</div>
		</>
	);
}

function SettingsSkeleton() {
	return (
		<>
			<LoadingLabel label="Loading settings…" />
			<div aria-hidden="true" {...stylex.props(styles.sections)}>
				<SectionSkeleton>
					<FieldSkeleton />
					<FieldSkeleton />
				</SectionSkeleton>
				<SectionSkeleton>
					<FieldSkeleton width={220} />
					<FieldSkeleton />
					<FieldSkeleton />
				</SectionSkeleton>
				<SectionSkeleton>
					<FieldSkeleton width={120} />
				</SectionSkeleton>
				<SectionSkeleton>
					<FieldSkeleton />
				</SectionSkeleton>
				<SectionSkeleton>
					<FieldSkeleton />
				</SectionSkeleton>
				<SectionSkeleton>
					<div {...stylex.props(styles.fieldGrid)}>
						<FieldSkeleton />
						<FieldSkeleton />
					</div>
				</SectionSkeleton>
				<SectionSkeleton>
					{[0, 1, 2].map((index) => (
						<div key={index} {...stylex.props(styles.row)}>
							<Chip width={14} height={14} />
							<Chip width={140} height={12} />
						</div>
					))}
					<div {...stylex.props(styles.fieldGrid)}>
						<FieldSkeleton />
						<FieldSkeleton />
					</div>
				</SectionSkeleton>
				<SectionSkeleton>
					<div {...stylex.props(styles.spread)}>
						<div {...stylex.props(styles.stack)}>
							<Chip width={120} height={12} />
							<Chip width={280} height={10} />
						</div>
						<Chip width={128} height={29} />
					</div>
				</SectionSkeleton>
			</div>
		</>
	);
}

/** Placeholder for the usage tab, toolbar included, while its code loads. */
export function UsagePanelSkeleton() {
	return (
		<>
			<div aria-hidden="true" {...stylex.props(usagePanelLayout.toolbar)}>
				<Chip width={92} height={12} />
				<span {...stylex.props(styles.spacer)} />
				<Chip width={66} height={30} />
				<Chip width={112} height={30} />
				<Chip width={30} height={30} />
			</div>
			<UsageChartsSkeleton />
		</>
	);
}

/** Placeholder for the usage chart cards, laid out like the real grid. */
export function UsageChartsSkeleton() {
	return (
		<>
			<LoadingLabel label="Loading usage…" />
			<div
				aria-hidden="true"
				{...stylex.props(usagePanelLayout.cardsContainer)}
			>
				<div {...stylex.props(usagePanelLayout.cards, usagePanelLayout.grid)}>
					{[0, 1, 2].map((index) => (
						<div key={index} {...stylex.props(usagePanelLayout.card)}>
							<div
								{...stylex.props(
									usagePanelLayout.cardHeader,
									styles.usageHeader,
								)}
							>
								<Chip width={64} height={12} />
								<span {...stylex.props(styles.spacer)} />
								<Chip width={84} height={10} />
							</div>
							<div {...stylex.props(usagePanelLayout.figure)}>
								<div {...stylex.props(usagePanelLayout.valueAxis)}>
									{[0, 1, 2, 3].map((tick) => (
										<Chip key={tick} width={36} height={8} />
									))}
								</div>
								<SkeletonChip
									styles={[usagePanelLayout.plot, styles.usagePlot]}
								/>
								<span />
								<div
									{...stylex.props(
										usagePanelLayout.timeAxis,
										styles.usageTimeAxis,
									)}
								>
									{[0, 1, 2].map((tick) => (
										<Chip key={tick} width={36} height={8} />
									))}
								</div>
							</div>
						</div>
					))}
				</div>
			</div>
		</>
	);
}

const styles = stylex.create({
	hidden: {
		position: "absolute",
		width: 1,
		height: 1,
		overflow: "hidden",
		clipPath: "inset(50%)",
		whiteSpace: "nowrap",
	},
	size: (width: number | string, height: number) => ({
		width,
		height,
		flexShrink: "0",
	}),
	spacer: { flex: "1" },
	stack: { display: "flex", flexDirection: "column", gap: space.sm },
	row: { display: "flex", alignItems: "center", gap: space.sm },
	spread: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.md,
	},
	// The gaps below mirror the real tab layouts.
	tab: { display: "flex", flexDirection: "column", gap: space.lg },
	sections: { display: "flex", flexDirection: "column", gap: "1.75rem" },
	sectionHeader: {
		display: "flex",
		flexDirection: "column",
		marginBottom: space.lg,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: "color-mix(in oklab, #fff 5%, transparent)",
		paddingBottom: space.md,
	},
	sectionTitle: { display: "flex", height: 24, alignItems: "center" },
	sectionLede: {
		display: "flex",
		height: 19,
		marginTop: 5,
		alignItems: "center",
	},
	sectionContent: { display: "flex", flexDirection: "column", gap: "0.875rem" },
	fieldLabel: {
		display: "flex",
		height: 13,
		marginBottom: "0.375rem",
		alignItems: "center",
	},
	fieldGrid: {
		display: "grid",
		gridTemplateColumns: {
			default: "repeat(2, minmax(0, 1fr))",
			"@media (width < 40rem)": "repeat(1, minmax(0, 1fr))",
		},
		gap: space.md,
	},
	deployments: {
		display: "flex",
		flexDirection: "column",
		gap: "0.625rem",
		padding: space.lg,
	},
	deploymentsMetadata: {
		display: "flex",
		minHeight: "1.75rem",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.lg,
	},
	deploymentCard: {
		display: "flex",
		minHeight: 52,
		alignItems: "center",
		gap: space.md,
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: "rgba(25,24,22,0.96)",
		paddingInline: { default: 28, "@media (max-width: 899px)": space.lg },
		paddingBlock: 10,
	},
	avatar: { height: 28, width: 28, flexShrink: "0", borderRadius: "50%" },
	deploymentCopy: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		flexDirection: "column",
		gap: space.xs,
	},
	variableList: { display: "flex", flexDirection: "column", gap: "0.625rem" },
	variablesEmpty: { display: "block", height: 84 },
	variableRow: {
		display: "grid",
		gridTemplateColumns: {
			default: "minmax(120px,0.8fr) minmax(160px,1.2fr) 32px",
			"@media (width < 40rem)": "1fr 32px",
		},
		alignItems: "flex-end",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
	},
	privateNetwork: {
		display: "flex",
		alignItems: "center",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		padding: "0.875rem",
	},
	bindingCard: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: "0.875rem",
		paddingBlock: space.md,
	},
	// Matches the line height of the real card title.
	usageHeader: { minHeight: 20, alignItems: "center" },
	usagePlot: { display: "block", opacity: 0.5 },
	usageTimeAxis: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-around",
	},
});
