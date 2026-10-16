import * as stylex from "@stylexjs/stylex";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, Download } from "lucide-react";
import { useState } from "react";
import { Badge } from "#/components/ui/badge";
import { Button } from "#/components/ui/button";
import { Notice } from "#/components/ui/notice";
import {
	formatCents,
	isUsageMeter,
	isZeroCents,
	meterLabel,
	statementKindLabel,
	statementLineExplanation,
	statementState,
} from "#/features/billing/billing-model";
import { SettingsCard } from "#/features/project-settings/settings-card";
import { WorkspacePageHeader } from "#/features/workspace/page-header";
import type {
	DashboardBillingStatement,
	DashboardBillingStatementLine,
	DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import { loadBillingStatement } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { formatDateTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function StatementList({
	workspaceId,
	statements,
}: {
	workspaceId: string;
	statements: Array<DashboardBillingStatement>;
}) {
	if (statements.length === 0)
		return (
			<p {...stylex.props(styles.muted)}>
				No statements yet. The first one is frozen a day after the current
				period ends.
			</p>
		);
	return (
		<ul {...stylex.props(styles.list)}>
			{statements.map((statement) => {
				const state = statementState(statement.state);
				return (
					<li key={statement.id} {...stylex.props(styles.listRow)}>
						<Link
							to="/workspace/billing/statements/$statementId"
							params={{ statementId: statement.id }}
							search={{ workspace: workspaceId }}
							{...stylex.props(styles.statementLink)}
						>
							{statementKindLabel(statement.kind)}
							<span {...stylex.props(styles.muted)}>
								{formatDateTime(statement.createdAt ?? "")}
							</span>
						</Link>
						<Badge tone={state.tone}>{state.label}</Badge>
						<span {...stylex.props(styles.amount)}>
							{formatCents(statement.amountDueCents, statement.currency)}
						</span>
						<HostedInvoiceLink statement={statement} />
					</li>
				);
			})}
		</ul>
	);
}

function HostedInvoiceLink({
	statement,
}: {
	statement: DashboardBillingStatement;
}) {
	if (!statement.stripeHostedInvoiceUrl) return <span />;
	return (
		<a
			href={statement.stripeHostedInvoiceUrl}
			target="_blank"
			rel="noreferrer"
			{...stylex.props(styles.link)}
		>
			{statement.state === "payment_failed" || statement.state === "open"
				? "Pay invoice"
				: "Invoice"}{" "}
			<ArrowUpRight size={11} />
		</a>
	);
}

/** Downloads the control plane's CSV rendering; replaceable in tests. */
export const statementDownload = {
	save(fileName: string, text: string) {
		const url = URL.createObjectURL(new Blob([text], { type: "text/csv" }));
		const link = document.createElement("a");
		link.href = url;
		link.download = fileName;
		link.click();
		URL.revokeObjectURL(url);
	},
};

export function StatementPage({
	shell,
	statement,
}: {
	shell: DashboardWorkspaceShell;
	statement: DashboardBillingStatement;
}) {
	const [downloading, setDownloading] = useState(false);
	const [error, setError] = useState<string>();
	const state = statementState(statement.state);
	const { currency } = statement;

	const downloadCsv = async () => {
		setDownloading(true);
		setError(undefined);
		try {
			const { statement: withCsv } = await loadBillingStatement({
				data: {
					workspaceId: shell.workspace.id,
					statementId: statement.id,
					includeCsv: true,
				},
			});
			statementDownload.save(`statement-${statement.id}.csv`, withCsv.csv);
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setDownloading(false);
		}
	};

	const totals: Array<[string, string]> = [
		["Gross usage", statement.grossCents],
		["Included usage", negate(statement.includedCents)],
		["Usage after allowance", statement.afterAllowanceCents],
		["Credit applied", negate(statement.creditCents)],
		["Monthly minimum", statement.minimumCents],
		["Minimum already prepaid", negate(statement.prepaidAppliedCents)],
		["Period total", statement.periodTotalCents],
		["Next period minimum", statement.nextMinimumCents],
	];

	return (
		<>
			<WorkspacePageHeader
				title={statementKindLabel(statement.kind)}
				lede={`Frozen ${formatDateTime(statement.createdAt ?? "")} for ${shell.workspace.name}`}
				actions={
					<>
						<Link
							to="/workspace/billing"
							search={{ workspace: shell.workspace.id }}
							{...stylex.props(styles.link)}
						>
							All billing
						</Link>
						<Button
							variant="ghost"
							disabled={downloading}
							onClick={() => void downloadCsv()}
						>
							<Download size={13} /> Download CSV
						</Button>
					</>
				}
			/>
			{error && <Notice>Could not download the CSV: {error}</Notice>}
			{statement.shadow && (
				<p {...stylex.props(styles.muted)}>
					Shadow preview: this statement shows what would have been billed. It
					was never charged.
				</p>
			)}

			<SettingsCard title="Amount due">
				<div {...stylex.props(styles.due)}>
					<span {...stylex.props(styles.dueAmount)}>
						{formatCents(statement.amountDueCents, currency)}
					</span>
					<Badge tone={state.tone}>{state.label}</Badge>
					<HostedInvoiceLink statement={statement} />
				</div>
				<dl {...stylex.props(styles.totals)}>
					{totals
						.filter(([, cents]) => !isZeroCents(cents))
						.map(([label, cents]) => (
							<div key={label} {...stylex.props(styles.totalRow)}>
								<dt>{label}</dt>
								<dd {...stylex.props(styles.amount)}>
									{formatCents(cents, currency)}
								</dd>
							</div>
						))}
				</dl>
				<p {...stylex.props(styles.muted)}>
					Amount due is the period total, less any minimum already prepaid, plus
					the next period's minimum and balances carried from earlier
					statements.
				</p>
			</SettingsCard>

			<SettingsCard
				title="Lines"
				lede="Every amount on the statement, with how it came about. Deleted projects keep their names."
			>
				<ul {...stylex.props(styles.list)}>
					{statement.lines.map((line) => (
						<StatementLineRow
							key={`${line.projectId}/${line.meter}/${line.periodStart ?? ""}`}
							line={line}
							currency={currency}
						/>
					))}
				</ul>
			</SettingsCard>
		</>
	);
}

function StatementLineRow({
	line,
	currency,
}: {
	line: DashboardBillingStatementLine;
	currency: string;
}) {
	const usage = isUsageMeter(line.meter);
	return (
		<li {...stylex.props(styles.line)}>
			<div {...stylex.props(styles.lineText)}>
				<strong {...stylex.props(styles.lineTitle)}>
					{usage
						? `${line.projectName || line.projectId} · ${meterLabel(line.meter)}`
						: line.description}
				</strong>
				{usage && (
					<span {...stylex.props(styles.muted)}>{line.description}</span>
				)}
				<span {...stylex.props(styles.muted)}>
					{statementLineExplanation(line, currency)}
				</span>
				{line.periodStart && (
					<span {...stylex.props(styles.dates)}>
						{formatDateTime(line.periodStart)} –{" "}
						{formatDateTime(line.periodEnd ?? "")}
					</span>
				)}
			</div>
			<span
				{...stylex.props(
					styles.amount,
					line.amountCents.startsWith("-") && styles.negative,
				)}
			>
				{formatCents(line.amountCents, currency)}
			</span>
		</li>
	);
}

function negate(cents: string): string {
	return isZeroCents(cents) ? "0" : (-BigInt(cents)).toString();
}

const styles = stylex.create({
	muted: {
		margin: 0,
		fontSize: "12px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	list: {
		margin: 0,
		padding: 0,
		listStyleType: "none",
		display: "flex",
		flexDirection: "column",
	},
	listRow: {
		display: "grid",
		gridTemplateColumns: "minmax(12rem, 1fr) auto minmax(6rem, auto) 6rem",
		alignItems: "center",
		gap: space.md,
		paddingBlock: space.sm,
		borderTopStyle: { default: "solid", ":first-child": "none" },
		borderTopWidth: "1px",
		borderTopColor: colors.line,
	},
	statementLink: {
		display: "flex",
		flexDirection: "column",
		fontSize: "13px",
		color: colors.ink,
		textDecoration: "none",
	},
	amount: {
		margin: 0,
		fontFamily: fonts.mono,
		fontSize: "13px",
		color: colors.ink,
		textAlign: "right",
	},
	negative: { color: colors.healthy },
	link: {
		display: "inline-flex",
		alignItems: "center",
		gap: "2px",
		fontSize: "12px",
		color: colors.accent,
		textDecoration: "none",
	},
	due: { display: "flex", alignItems: "center", gap: space.md },
	dueAmount: {
		fontFamily: fonts.display,
		fontSize: "32px",
		color: colors.ink,
	},
	totals: {
		margin: 0,
		display: "flex",
		flexDirection: "column",
		gap: space.xs,
		fontSize: "13px",
		color: colors.label,
	},
	totalRow: { display: "flex", justifyContent: "space-between" },
	line: {
		display: "flex",
		alignItems: "flex-start",
		justifyContent: "space-between",
		gap: space.lg,
		paddingBlock: space.sm,
		borderTopStyle: { default: "solid", ":first-child": "none" },
		borderTopWidth: "1px",
		borderTopColor: colors.line,
	},
	lineText: {
		display: "flex",
		minWidth: 0,
		flexDirection: "column",
		gap: "2px",
	},
	lineTitle: { fontSize: "13px", color: colors.ink },
	dates: { fontFamily: fonts.mono, fontSize: "11px", color: colors.dim },
});
