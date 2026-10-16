import * as stylex from "@stylexjs/stylex";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, Ban, Eye, TriangleAlert } from "lucide-react";
import {
	billingRestrictionReason,
	formatResourceTime,
	type RestrictionView,
	restrictionView,
} from "#/features/billing/billing-model";
import type {
	DashboardBillingStatement,
	DashboardBillingSummary,
} from "#/lib/dashboard/core/types.server";
import { formatDateTime } from "#/lib/time";
import { colors, fonts, shape, space } from "#/styles/tokens.stylex";

/**
 * Watermark and gap indicator shown beside every estimate: how far usage is
 * complete and how much resource time went unmeasured.
 */
export function EstimateCoverage({
	summary,
}: {
	summary: Pick<
		DashboardBillingSummary,
		"watermark" | "gapMs" | "uncertainMs" | "measuredMs"
	>;
}) {
	const gap = summary.gapMs !== "0";
	const uncertain = summary.uncertainMs !== "0";
	const warn = gap || !summary.watermark;
	return (
		<span
			data-testid="estimate-coverage"
			{...stylex.props(styles.coverage, warn && styles.coverageWarn)}
		>
			{warn && <TriangleAlert size={11} {...stylex.props(styles.icon)} />}
			<span>
				{summary.watermark
					? `Complete through ${formatDateTime(summary.watermark)}`
					: "No usage reported yet"}
				{gap && ` · ${formatResourceTime(summary.gapMs)} unmeasured`}
				{uncertain &&
					` · ${formatResourceTime(summary.uncertainMs)} with uncertain timing`}
			</span>
		</span>
	);
}

const blockedActions =
	"builds, environment releases, restarts and redeploys, scaling up, and volume creation or growth";

/** Explains a billing restriction without overstating a shadow preview. */
export function RestrictionNotice({
	summary,
	invoice,
	compact = false,
}: {
	summary: Pick<DashboardBillingSummary, "restriction">;
	/** A hosted invoice that settles a failed payment, when one exists. */
	invoice?: DashboardBillingStatement;
	compact?: boolean;
}) {
	const view = restrictionView(summary);
	if (view.kind === "none") return null;
	if (view.kind === "shadow")
		return (
			<output {...stylex.props(styles.notice, styles.shadow)}>
				<Eye size={14} {...stylex.props(styles.icon)} />
				<span>
					<strong>Shadow preview — would restrict</strong>
					{reasonSuffix(view)}. Billing is in shadow mode, so nothing is blocked
					or stopped.
				</span>
			</output>
		);
	const stopped = view.kind === "stopped";
	return (
		<div role="alert" {...stylex.props(styles.notice, styles.enforced)}>
			<Ban size={14} {...stylex.props(styles.icon)} />
			<span {...stylex.props(styles.body)}>
				<strong>
					{stopped
						? "Services are stopped by a billing restriction"
						: "New usage is blocked by a billing restriction"}
				</strong>
				{reasonSuffix(view)}.
				{!compact && (
					<>
						{" "}
						{stopped
							? `Running replicas are drained, and ${blockedActions} are blocked.`
							: `Running services keep running, but ${blockedActions} are blocked.`}{" "}
						Drafts and desired replica counts are kept, so service resumes when
						the restriction clears. You can still inspect, pay, export, scale
						down, and delete.
						{view.kind === "growthBlocked" &&
							" Compute stops if the payment is still failed 7 days after it failed."}
					</>
				)}
				{invoice?.stripeHostedInvoiceUrl && (
					<>
						{" "}
						<a
							href={invoice.stripeHostedInvoiceUrl}
							target="_blank"
							rel="noreferrer"
							{...stylex.props(styles.link)}
						>
							Pay the outstanding invoice <ArrowUpRight size={11} />
						</a>
					</>
				)}
			</span>
		</div>
	);
}

function reasonSuffix(view: Exclude<RestrictionView, { kind: "none" }>) {
	const since = view.since ? ` since ${formatDateTime(view.since)}` : "";
	return view.reason ? `: ${view.reason}${since}` : since;
}

/**
 * Appended to an action's error when the control plane refused it with a
 * billing restriction; renders nothing for any other error.
 */
export function BillingRestrictionHint({ message }: { message?: string }) {
	if (!message || billingRestrictionReason(message) === undefined) return null;
	return (
		<span {...stylex.props(styles.hint)}>
			This workspace's billing restriction blocks new usage. Drafts and desired
			replica counts are kept and apply once it clears.{" "}
			<Link to="/workspace/billing" {...stylex.props(styles.link)}>
				Open billing <ArrowUpRight size={11} />
			</Link>
		</span>
	);
}

const styles = stylex.create({
	coverage: {
		display: "inline-flex",
		alignItems: "center",
		gap: space.xs,
		fontFamily: fonts.mono,
		fontSize: "11px",
		lineHeight: "1.4",
		color: colors.dim,
	},
	coverageWarn: { color: colors.building },
	icon: { flexShrink: 0, marginTop: "2px" },
	notice: {
		display: "flex",
		alignItems: "flex-start",
		gap: space.sm,
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		paddingInline: space.md,
		paddingBlock: space.sm,
		fontSize: "13px",
		lineHeight: "1.5",
	},
	shadow: {
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		color: colors.muted,
	},
	enforced: {
		borderColor: "rgba(208,85,85,0.4)",
		backgroundColor: colors.failedDim,
		color: colors.ink,
	},
	body: { minWidth: 0 },
	link: {
		display: "inline-flex",
		alignItems: "center",
		gap: "2px",
		color: colors.accent,
		textDecoration: "none",
	},
	hint: {
		display: "block",
		marginTop: space.xs,
		fontFamily: fonts.sans,
		fontSize: "12px",
		color: colors.ink,
	},
});
