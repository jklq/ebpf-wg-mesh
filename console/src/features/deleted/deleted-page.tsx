import * as stylex from "@stylexjs/stylex";
import { PageShell } from "#/components/layout/page-shell";
import { Button } from "#/components/ui/button";
import { noticeStyles } from "#/components/ui/notice";
import { textStyles } from "#/components/ui/text";
import {
	groupResources,
	kindMeta,
	resourceKey,
} from "#/features/deleted/deleted-resource-model";
import { DeletedRow } from "#/features/deleted/deleted-resource-row";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { useRouter } from "@tanstack/react-router";
import { History, Layers, RefreshCw } from "lucide-react";
import { useMemo, useState } from "react";
import type { DashboardDeletedResource } from "#/lib/dashboard/core/types.server";
import { doRestoreResource } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";

export function RecentlyDeletedPage({
	state,
}: {
	state: { resources: DashboardDeletedResource[]; nowMs: number };
}) {
	const router = useRouter();
	const { resources, nowMs } = state;
	const [refreshing, setRefreshing] = useState(false);
	const [restoringKey, setRestoringKey] = useState<string>();
	const [errors, setErrors] = useState<Record<string, string>>({});
	const [restored, setRestored] = useState<string>();

	const { projects, childrenOf } = useMemo(
		() => groupResources(resources),
		[resources],
	);

	const refresh = async () => {
		setRefreshing(true);
		try {
			await router.invalidate();
		} finally {
			setRefreshing(false);
		}
	};

	const restore = async (entry: DashboardDeletedResource) => {
		if (entry.kind === "volume") return;
		const key = resourceKey(entry);
		setRestoringKey(key);
		setRestored(undefined);
		setErrors((current) => ({ ...current, [key]: "" }));
		try {
			await doRestoreResource({ data: { kind: entry.kind, id: entry.id } });
			setRestored(`${kindMeta[entry.kind].label} ${entry.name} was restored.`);
			await router.invalidate();
		} catch (cause) {
			setErrors((current) => ({ ...current, [key]: formatError(cause) }));
		} finally {
			setRestoringKey(undefined);
		}
	};

	const renderRow = (entry: DashboardDeletedResource, depth: number) => {
		const key = resourceKey(entry);
		const children = childrenOf.get(key) ?? [];
		return (
			<li key={key} {...stylex.props(styles.resourceTreeItem)}>
				<DeletedRow
					entry={entry}
					depth={depth}
					nowMs={nowMs}
					restoring={restoringKey === key}
					disabled={Boolean(restoringKey)}
					error={errors[key]}
					onRestore={() => void restore(entry)}
				/>
				{children.length > 0 && (
					<ul {...stylex.props(styles.childResources)}>
						{children.map((child) => renderRow(child, depth + 1))}
					</ul>
				)}
			</li>
		);
	};

	return (
		<PageShell
			title="Recently deleted"
			icon={<History size={15} />}
			actions={
				<Button
					type="button"
					variant="ghost"
					onClick={() => void refresh()}
					disabled={refreshing}
				>
					<RefreshCw
						size={13}
						{...stylex.props(refreshing ? styles.refreshing : undefined)}
					/>{" "}
					Refresh
				</Button>
			}
		>
			<p {...stylex.props(textStyles.eyebrow)}>Recovery</p>
			<h1 {...stylex.props(styles.title)}>Recently deleted</h1>
			<p {...stylex.props(styles.description)}>
				Deleted resources stop serving right away and stay restorable for a
				grace period. After that they are destroyed for good. Volumes cannot be
				restored.
			</p>

			{restored && (
				<output
					{...stylex.props([noticeStyles.success, styles.restoredMessage])}
				>
					{restored}
				</output>
			)}

			{projects.length === 0 ? (
				<div {...stylex.props(styles.emptyState)}>
					<History size={20} {...stylex.props(styles.emptyIcon)} />
					<strong {...stylex.props(styles.emptyTitle)}>Nothing deleted</strong>
					<span {...stylex.props(styles.emptyDescription)}>
						Deleted projects, environments, services, domains, and volumes show
						up here during their grace period.
					</span>
				</div>
			) : (
				<div {...stylex.props(styles.projectGroups)}>
					{projects.map((group) => (
						<section key={group.projectId} aria-label={group.projectName}>
							<h2 {...stylex.props(styles.projectTitle)}>
								<Layers size={12} />
								{group.projectName}
							</h2>
							<ul {...stylex.props(styles.rootResources)}>
								{group.roots.map((entry) => renderRow(entry, 0))}
							</ul>
						</section>
					))}
				</div>
			)}
		</PageShell>
	);
}

/** Groups by project, nesting each resource under its nearest listed tombstoned ancestor. */

const styles = stylex.create({
	resourceTreeItem: { display: "flex", flexDirection: "column" },
	childResources: {
		margin: "0rem",
		display: "flex",
		listStyleType: "none",
		flexDirection: "column",
		padding: "0rem",
	},
	refreshing: { animation: `${spin} 1s linear infinite` },
	title: {
		marginTop: "0.125rem",
		marginBottom: "5px",
		fontFamily: fonts.display,
		fontSize: "32px",
		fontWeight: "500",
	},
	description: { margin: "0rem", maxWidth: "620px", color: colors.muted },
	restoredMessage: {
		marginTop: space.xl,
		marginBottom: "0rem",
		display: "block",
	},
	emptyState: {
		marginTop: space.xxl,
		display: "flex",
		flexDirection: "column",
		alignItems: "center",
		gap: "0.375rem",
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.xl,
		paddingBlock: "3.5rem",
		textAlign: "center",
	},
	emptyIcon: { color: colors.dim },
	emptyTitle: {
		marginTop: space.xs,
		fontFamily: fonts.display,
		fontSize: "1.125rem",
		lineHeight: "calc(1.75 / 1.125)",
		fontWeight: "500",
		color: colors.ink,
	},
	emptyDescription: { fontSize: "13px", color: colors.muted },
	projectGroups: {
		marginTop: space.xxl,
		display: "flex",
		flexDirection: "column",
		gap: "1.75rem",
	},
	projectTitle: {
		margin: "0rem",
		marginBottom: "0.625rem",
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		fontFamily: fonts.condensed,
		fontSize: "12px",
		fontWeight: "700",
		letterSpacing: "0.1em",
		color: colors.muted,
		textTransform: "uppercase",
	},
	rootResources: {
		margin: "0rem",
		display: "flex",
		listStyleType: "none",
		flexDirection: "column",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		padding: "0rem",
	},
});
