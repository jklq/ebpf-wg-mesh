import { createFileRoute, useRouter } from "@tanstack/react-router";
import {
	ArrowLeft,
	Box,
	Database,
	Globe,
	History,
	Layers,
	Loader2,
	RefreshCw,
	RotateCcw,
	Server,
} from "lucide-react";
import { type ComponentType, type ReactNode, useMemo, useState } from "react";

import { cn } from "#/lib/cn";
import type {
	DashboardDeletedResource,
	DashboardDeletedResourceKind,
} from "#/lib/dashboard/core/types.server";
import {
	cleanDate,
	formatDateTime,
	formatRelativeTime,
	formatTimeUntil,
} from "#/lib/time";
import {
	btnGhost,
	btnSecondary,
	errorMsg,
	panelEyebrow,
	successMsg,
} from "#/lib/ui-classes";
import {
	doRestoreResource,
	loadRecentlyDeleted,
} from "./-dashboard/server-fns";
import { formatError } from "./-dashboard/service-utils";

export const Route = createFileRoute("/deleted")({
	loader: async () => ({
		resources: await loadRecentlyDeleted(),
		nowMs: Date.now(),
	}),
	component: RecentlyDeletedRoute,
});

const kindMeta: Record<
	DashboardDeletedResourceKind,
	{ label: string; icon: ComponentType<{ size?: number; className?: string }> }
> = {
	project: { label: "Project", icon: Layers },
	environment: { label: "Environment", icon: Server },
	service: { label: "Service", icon: Box },
	domain: { label: "Domain", icon: Globe },
	volume: { label: "Volume", icon: Database },
};

function resourceKey(entry: { kind: string; id: string }): string {
	return `${entry.kind}:${entry.id}`;
}

function RecentlyDeletedRoute() {
	const router = useRouter();
	const { resources, nowMs } = Route.useLoaderData();
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
			<li key={key} className="flex flex-col">
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
					<ul className="m-0 flex list-none flex-col p-0">
						{children.map((child) => renderRow(child, depth + 1))}
					</ul>
				)}
			</li>
		);
	};

	return (
		<main className="min-h-dvh bg-canvas">
			<header className="sticky top-0 z-30 flex h-header items-center gap-2.5 border-b border-line bg-surface px-4">
				<a href="/" className={btnGhost}>
					<ArrowLeft size={13} /> Services
				</a>
				<div className="ml-2 flex items-center gap-[7px]">
					<History size={15} />
					<strong>Recently deleted</strong>
				</div>
				<div className="flex-1" />
				<button
					type="button"
					className={btnGhost}
					onClick={() => void refresh()}
					disabled={refreshing}
				>
					<RefreshCw size={13} className={refreshing ? "animate-spin" : ""} />{" "}
					Refresh
				</button>
			</header>

			<section className="mx-auto w-[min(920px,calc(100%-32px))] py-9 pb-18">
				<p className={panelEyebrow}>Recovery</p>
				<h1 className="mt-0.5 mb-[5px] font-display text-[32px] font-medium">
					Recently deleted
				</h1>
				<p className="m-0 max-w-[620px] text-muted">
					Deleted resources stop serving right away and stay restorable for a
					grace period. After that they are destroyed for good. Volumes cannot
					be restored.
				</p>

				{restored && (
					<output className={cn(successMsg, "mt-6 mb-0 block")}>
						{restored}
					</output>
				)}

				{projects.length === 0 ? (
					<div className="mt-8 flex flex-col items-center gap-1.5 border border-dashed border-line px-6 py-14 text-center">
						<History size={20} className="text-dim" />
						<strong className="mt-1 font-display text-lg font-medium text-ink">
							Nothing deleted
						</strong>
						<span className="text-[13px] text-muted">
							Deleted projects, environments, services, domains, and volumes
							show up here during their grace period.
						</span>
					</div>
				) : (
					<div className="mt-8 flex flex-col gap-7">
						{projects.map((group) => (
							<section key={group.projectId} aria-label={group.projectName}>
								<h2 className="m-0 mb-2.5 flex items-center gap-2 font-condensed text-[12px] font-bold tracking-[0.1em] text-muted uppercase">
									<Layers size={12} />
									{group.projectName}
								</h2>
								<ul className="m-0 flex list-none flex-col border border-line bg-surface p-0">
									{group.roots.map((entry) => renderRow(entry, 0))}
								</ul>
							</section>
						))}
					</div>
				)}
			</section>
		</main>
	);
}

function DeletedRow({
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
			<span className="border border-[rgba(184,66,66,0.3)] bg-failed-dim px-2 py-1 font-condensed text-[10px] font-bold tracking-[0.08em] text-failed uppercase">
				Not recoverable
			</span>
		);
	} else if (parent && entry.deletion.inherited) {
		action = (
			<span className="max-w-[180px] text-right text-[11px] leading-snug text-dim">
				Returns when {parent.name} is restored
			</span>
		);
	} else if (parent) {
		action = (
			<span className="max-w-[180px] text-right text-[11px] leading-snug text-dim">
				Restore {parent.name} first
			</span>
		);
	} else {
		action = (
			<button
				type="button"
				className={btnSecondary}
				onClick={onRestore}
				disabled={disabled}
				aria-label={`Restore ${meta.label.toLowerCase()} ${entry.name}`}
			>
				{restoring ? (
					<Loader2 size={12} className="animate-spin" />
				) : (
					<RotateCcw size={12} />
				)}
				{restoring ? "Restoring…" : "Restore"}
			</button>
		);
	}

	return (
		<div
			className={cn(
				"flex flex-col gap-2 border-t border-line px-4 py-3 first:border-t-0",
				depth > 0 && "bg-canvas/60",
			)}
			style={depth > 0 ? { paddingLeft: 16 + depth * 26 } : undefined}
		>
			<div className="flex items-center gap-3 max-sm:flex-col max-sm:items-start">
				<span
					className={cn(
						"inline-flex size-8 shrink-0 items-center justify-center border",
						entry.kind === "volume"
							? "border-[rgba(184,66,66,0.3)] text-failed"
							: "border-line-bright text-muted",
					)}
				>
					<Icon size={14} />
				</span>
				<div className="flex min-w-0 flex-1 flex-col gap-0.5">
					<div className="flex min-w-0 items-center gap-2">
						<strong
							className={cn(
								"truncate text-[14px] font-semibold text-ink",
								entry.kind === "domain" && "font-mono text-[13px]",
							)}
						>
							{entry.name}
						</strong>
						<span className="shrink-0 font-condensed text-[10px] font-bold tracking-[0.1em] text-dim uppercase">
							{meta.label}
						</span>
					</div>
					<span className="text-xs text-muted">
						{context && <span>{context} · </span>}
						Deleted {formatRelativeTime(entry.deletion.deletedAt, nowMs, "—")}
						{expiresAt && (
							<>
								{" · "}
								<span
									className={
										entry.kind === "volume" ? "text-failed" : "text-muted"
									}
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
				<div className="flex shrink-0 items-center">{action}</div>
			</div>
			{error && (
				<p className={cn(errorMsg, "m-0")} role="alert">
					{error}
				</p>
			)}
		</div>
	);
}

/**
 * Groups by project and nests each resource under its nearest tombstoned
 * ancestor when that ancestor is listed, so a restore reads top-down.
 */
function groupResources(resources: Array<DashboardDeletedResource>) {
	const keys = new Set(resources.map(resourceKey));
	const childrenOf = new Map<string, Array<DashboardDeletedResource>>();
	const projects: Array<{
		projectId: string;
		projectName: string;
		roots: Array<DashboardDeletedResource>;
	}> = [];
	for (const raw of resources) {
		const entry = {
			...raw,
			deletion: {
				...raw.deletion,
				deletedAt: cleanDate(raw.deletion.deletedAt),
				deleteExpiresAt: cleanDate(raw.deletion.deleteExpiresAt),
			},
		};
		const parentKey = entry.deletedWith
			? resourceKey(entry.deletedWith)
			: undefined;
		if (parentKey && keys.has(parentKey)) {
			childrenOf.set(parentKey, [...(childrenOf.get(parentKey) ?? []), entry]);
			continue;
		}
		let group = projects.find((item) => item.projectId === entry.projectId);
		if (!group) {
			group = {
				projectId: entry.projectId,
				projectName: entry.projectName,
				roots: [],
			};
			projects.push(group);
		}
		group.roots.push(entry);
	}
	return { projects, childrenOf };
}
