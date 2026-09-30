import * as stylex from "@stylexjs/stylex";
import { Loader2, RotateCcw } from "lucide-react";
import type { ReactNode } from "react";
import { Button } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";
import { kindMeta } from "#/features/deleted/deleted-resource-model";
import type { DashboardDeletedResource } from "#/lib/dashboard/core/types.server";
import {
	formatDateTime,
	formatRelativeTime,
	formatTimeUntil,
} from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
const styles = stylex.create({
	indentation: (paddingLeft: number) => ({ paddingLeft }),
	refreshing: { animation: `${spin} 1s linear infinite` },
	unrecoverableBadge: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(184,66,66,0.3)",
		backgroundColor: colors.failedDim,
		paddingInline: space.sm,
		paddingBlock: space.xs,
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		letterSpacing: "0.08em",
		color: colors.failed,
		textTransform: "uppercase",
	},
	parentRecoveryHint: {
		maxWidth: "180px",
		textAlign: "right",
		fontSize: "11px",
		lineHeight: "1.375",
		color: colors.dim,
	},
	resourceRow: {
		display: "flex",
		flexDirection: "column",
		gap: space.sm,
		borderTopStyle: { default: "solid", ":first-child": "solid" },
		borderTopWidth: { default: "1px", ":first-child": "0px" },
		borderColor: colors.line,
		paddingInline: space.lg,
		paddingBlock: space.md,
	},
	nestedRow: {
		backgroundColor: `color-mix(in oklab, ${colors.canvas} 60%, transparent)`,
	},
	resourceSummary: {
		display: "flex",
		alignItems: { default: "center", "@media (width < 40rem)": "flex-start" },
		gap: space.md,
		flexDirection: { default: null, "@media (width < 40rem)": "column" },
	},
	resourceIcon: {
		display: "inline-flex",
		width: "2rem",
		height: "2rem",
		flexShrink: "0",
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "1px",
	},
	unrecoverableIcon: {
		borderColor: "rgba(184,66,66,0.3)",
		color: colors.failed,
	},
	recoverableIcon: { borderColor: colors.lineBright, color: colors.muted },
	resourceCopy: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		flexDirection: "column",
		gap: "0.125rem",
	},
	resourceHeading: {
		display: "flex",
		minWidth: "0rem",
		alignItems: "center",
		gap: space.sm,
	},
	resourceName: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontSize: "14px",
		fontWeight: "600",
		color: colors.ink,
	},
	mutedResourceName: { fontFamily: fonts.mono, fontSize: "13px" },
	resourceKind: {
		flexShrink: "0",
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		letterSpacing: "0.1em",
		color: colors.dim,
		textTransform: "uppercase",
	},
	resourceMetadata: {
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	imminentExpiry: { color: colors.failed },
	expiry: { color: colors.muted },
	rowActions: { display: "flex", flexShrink: "0", alignItems: "center" },
	rowError: { margin: "0rem" },
});
export function DeletedRow({
	entry,
	depth,
	nowMs,
	restoring,
	disabled,
	error,
	onRestore,
}: {
	entry: DashboardDeletedResource;
	depth: number;
	nowMs: number;
	restoring: boolean;
	disabled: boolean;
	error?: string;
	onRestore: () => void;
}) {
	const meta = kindMeta[entry.kind];
	const Icon = meta.icon;
	const expiresAt = entry.deletion.deleteExpiresAt;
	const context = [
		entry.environmentName && entry.kind !== "environment"
			? entry.environmentName
			: undefined,
		entry.serviceName,
	]
		.filter(Boolean)
		.join(" / ");
	const parent = entry.deletedWith;

	let action: ReactNode;
	if (entry.kind === "volume") {
		action = (
			<span {...stylex.props(styles.unrecoverableBadge)}>Not recoverable</span>
		);
	} else if (parent && entry.deletion.inherited) {
		action = (
			<span {...stylex.props(styles.parentRecoveryHint)}>
				Returns when {parent.name} is restored
			</span>
		);
	} else if (parent) {
		action = (
			<span {...stylex.props(styles.parentRecoveryHint)}>
				Restore {parent.name} first
			</span>
		);
	} else {
		action = (
			<Button
				type="button"
				variant="secondary"
				onClick={onRestore}
				disabled={disabled}
				aria-label={`Restore ${meta.label.toLowerCase()} ${entry.name}`}
			>
				{restoring ? (
					<Loader2 size={12} {...stylex.props(styles.refreshing)} />
				) : (
					<RotateCcw size={12} />
				)}
				{restoring ? "Restoring…" : "Restore"}
			</Button>
		);
	}

	return (
		<div
			{...stylex.props([
				styles.resourceRow,
				depth > 0 && [styles.nestedRow, styles.indentation(16 + depth * 26)],
			])}
		>
			<div {...stylex.props(styles.resourceSummary)}>
				<span
					{...stylex.props([
						styles.resourceIcon,
						entry.kind === "volume"
							? styles.unrecoverableIcon
							: styles.recoverableIcon,
					])}
				>
					<Icon size={14} />
				</span>
				<div {...stylex.props(styles.resourceCopy)}>
					<div {...stylex.props(styles.resourceHeading)}>
						<strong
							{...stylex.props([
								styles.resourceName,
								entry.kind === "domain" && styles.mutedResourceName,
							])}
						>
							{entry.name}
						</strong>
						<span {...stylex.props(styles.resourceKind)}>{meta.label}</span>
					</div>
					<span {...stylex.props(styles.resourceMetadata)}>
						{context && <span>{context} · </span>}
						Deleted {formatRelativeTime(entry.deletion.deletedAt, nowMs, "—")}
						{expiresAt && (
							<>
								{" · "}
								<span
									{...stylex.props(
										entry.kind === "volume"
											? styles.imminentExpiry
											: styles.expiry,
									)}
									title={formatDateTime(expiresAt)}
								>
									{entry.kind === "volume" ? "destroyed" : "restorable until"}{" "}
									{entry.kind === "volume"
										? formatTimeUntil(expiresAt, nowMs)
										: formatDateTime(expiresAt)}
								</span>
							</>
						)}
					</span>
				</div>
				<div {...stylex.props(styles.rowActions)}>{action}</div>
			</div>
			{error && (
				<p
					{...stylex.props([noticeStyles.error, styles.rowError])}
					role="alert"
				>
					{error}
				</p>
			)}
		</div>
	);
}
