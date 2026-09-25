import { createFileRoute, useRouter } from "@tanstack/react-router";
import {
	ArrowLeft,
	Check,
	Database,
	History,
	Loader2,
	Settings,
	Trash2,
} from "lucide-react";
import { type FormEvent, type ReactNode, useEffect, useState } from "react";

import { cn } from "#/lib/cn";
import type {
	DashboardProject,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { cleanDate, formatRelativeTime } from "#/lib/time";
import {
	btnDangerOutline,
	btnGhost,
	btnPrimary,
	errorMsg,
	fieldInput,
	fieldLabel,
	panelEyebrow,
	panelIconBtn,
} from "#/lib/ui-classes";
import { DeleteDialog } from "../../-dashboard/delete-dialog";
import {
	doDeleteResource,
	doUpdateProjectLogRetention,
	fetchDeletionPreview,
	loadProjectSettings,
} from "../../-dashboard/server-fns";
import { formatError } from "../../-dashboard/service-utils";

export const Route = createFileRoute("/projects/$projectId/settings")({
	loader: async ({ params }) => ({
		...(await loadProjectSettings({ data: { projectId: params.projectId } })),
		nowMs: Date.now(),
	}),
	component: ProjectSettingsRoute,
});

type PendingDelete =
	| { kind: "project"; id: string; name: string }
	| { kind: "volume"; id: string; name: string; environmentName: string };

function ProjectSettingsRoute() {
	const router = useRouter();
	const settings = Route.useLoaderData();
	const { project } = settings;
	const [pendingDelete, setPendingDelete] = useState<PendingDelete>();
	const [deleting, setDeleting] = useState(false);
	const [deleteError, setDeleteError] = useState<string>();
	const volumeCount = settings.environments.reduce(
		(total, entry) => total + entry.volumes.length,
		0,
	);

	const confirmDelete = async (confirmationName: string) => {
		if (!pendingDelete) return;
		setDeleting(true);
		setDeleteError(undefined);
		try {
			await doDeleteResource({
				data: {
					kind: pendingDelete.kind,
					id: pendingDelete.id,
					confirmationName,
				},
			});
			setPendingDelete(undefined);
			if (pendingDelete.kind === "project") {
				await router.navigate({ to: "/deleted" });
				return;
			}
			await router.invalidate();
		} catch (cause) {
			setDeleteError(formatError(cause));
		} finally {
			setDeleting(false);
		}
	};

	return (
		<main className="min-h-dvh bg-canvas">
			<header className="sticky top-0 z-30 flex h-header items-center gap-2.5 border-b border-line bg-surface px-4">
				<a href={`/projects/${project.id}`} className={btnGhost}>
					<ArrowLeft size={13} /> {project.name}
				</a>
				<div className="ml-2 flex items-center gap-[7px]">
					<Settings size={15} />
					<strong>Project settings</strong>
				</div>
				<div className="flex-1" />
				<a href="/deleted" className={cn(btnGhost, "gap-1 text-xs")}>
					<History size={13} /> Recently deleted
				</a>
			</header>

			<section className="mx-auto flex w-[min(820px,calc(100%-32px))] flex-col gap-6 py-9 pb-18">
				<div>
					<p className={panelEyebrow}>Project</p>
					<h1 className="mt-0.5 mb-[5px] font-display text-[32px] font-medium">
						{project.name}
					</h1>
					<p className="m-0 font-mono text-xs text-dim">{project.id}</p>
				</div>

				<SettingsCard
					title="Log retention"
					lede="How long runtime, build, and deploy logs are kept for every service in this project."
				>
					<LogRetentionForm project={project} />
				</SettingsCard>

				<SettingsCard
					title="Volumes"
					lede="Persistent disks attached to services. Deleting a volume destroys its data when the grace period ends; volumes cannot be restored."
				>
					{volumeCount === 0 ? (
						<p className="m-0 border border-dashed border-line px-4 py-5 text-center text-[13px] text-muted">
							No volumes in this project.
						</p>
					) : (
						<div className="flex flex-col gap-4">
							{settings.environments
								.filter((entry) => entry.volumes.length > 0)
								.map(({ environment, volumes }) => (
									<div key={environment.id} className="flex flex-col gap-2">
										<span className="flex items-center gap-2 font-condensed text-[11px] font-bold tracking-[0.1em] text-muted uppercase">
											{environment.name}
											{environment.isProduction && (
												<span className="rounded-[1px] border border-accent-glow bg-accent-dim px-[5px] py-px text-[9px] text-accent">
													prod
												</span>
											)}
										</span>
										<ul className="m-0 flex list-none flex-col border border-line p-0">
											{volumes.map((volume) => (
												<VolumeRow
													nowMs={settings.nowMs}
													key={volume.id}
													volume={volume}
													onDelete={() => {
														setDeleteError(undefined);
														setPendingDelete({
															kind: "volume",
															id: volume.id,
															name: volume.name,
															environmentName: environment.name,
														});
													}}
												/>
											))}
										</ul>
									</div>
								))}
						</div>
					)}
				</SettingsCard>

				<SettingsCard title="Danger zone" tone="danger">
					<div className="flex items-center justify-between gap-4 max-sm:flex-col max-sm:items-start">
						<div>
							<strong className="mb-[3px] block text-[13px] font-semibold text-ink">
								Delete this project
							</strong>
							<span className="block text-xs leading-normal text-muted">
								Stops every service in every environment and removes their
								domains. You can restore the project from Recently deleted
								during the grace period.
							</span>
						</div>
						<button
							type="button"
							className={btnDangerOutline}
							onClick={() => {
								setDeleteError(undefined);
								setPendingDelete({
									kind: "project",
									id: project.id,
									name: project.name,
								});
							}}
						>
							<Trash2 size={13} />
							Delete project
						</button>
					</div>
				</SettingsCard>
			</section>

			{pendingDelete?.kind === "project" && (
				<DeleteDialog
					title="Delete Project"
					name={pendingDelete.name}
					recovery="restorable"
					requireName
					loadPreview={() =>
						fetchDeletionPreview({
							data: { kind: "project", id: pendingDelete.id },
						})
					}
					busy={deleting}
					error={deleteError}
					description={
						<>
							You are <span className="text-failed">deleting</span> the project{" "}
							<strong>{pendingDelete.name}</strong>. Everything in it stops
							serving immediately.
						</>
					}
					onCancel={() => {
						if (deleting) return;
						setPendingDelete(undefined);
					}}
					onConfirm={(confirmationName) => void confirmDelete(confirmationName)}
				/>
			)}
			{pendingDelete?.kind === "volume" && (
				<DeleteDialog
					title="Delete Volume"
					name={pendingDelete.name}
					recovery="permanent"
					requireName
					previewServicesLabel="Mounted by"
					loadPreview={() =>
						fetchDeletionPreview({
							data: { kind: "volume", id: pendingDelete.id },
						})
					}
					busy={deleting}
					error={deleteError}
					description={
						<>
							You are <span className="text-failed">deleting</span> the volume{" "}
							<strong>{pendingDelete.name}</strong> in{" "}
							{pendingDelete.environmentName}. A volume that a service still
							mounts cannot be deleted.
						</>
					}
					onCancel={() => {
						if (deleting) return;
						setPendingDelete(undefined);
					}}
					onConfirm={(confirmationName) => void confirmDelete(confirmationName)}
				/>
			)}
		</main>
	);
}

function SettingsCard({
	title,
	lede,
	tone = "default",
	children,
}: {
	title: string;
	lede?: string;
	tone?: "default" | "danger";
	children: ReactNode;
}) {
	return (
		<section
			className={cn(
				"flex flex-col gap-4 border bg-surface p-5",
				tone === "danger" ? "border-[rgba(184,66,66,0.4)]" : "border-line",
			)}
		>
			<header>
				<h2
					className={cn(
						"m-0 font-display text-[20px] font-medium tracking-tight",
						tone === "danger" ? "text-failed" : "text-ink",
					)}
				>
					{title}
				</h2>
				{lede && (
					<p className="mt-1 mb-0 text-[13px] leading-[1.45] text-muted">
						{lede}
					</p>
				)}
			</header>
			{children}
		</section>
	);
}

function VolumeRow({
	nowMs,
	volume,
	onDelete,
}: {
	volume: DashboardVolume;
	nowMs: number;
	onDelete: () => void;
}) {
	const createdAt = cleanDate(volume.createdAt);
	return (
		<li className="flex items-center gap-3 border-t border-line px-3.5 py-2.5 first:border-t-0">
			<Database size={14} className="shrink-0 text-muted" />
			<div className="flex min-w-0 flex-1 flex-col">
				<strong className="truncate font-mono text-[13px] font-medium text-ink">
					{volume.name}
				</strong>
				<span className="text-[11px] text-dim">
					{formatBytes(volume.sizeBytes)}
					{createdAt && ` · created ${formatRelativeTime(createdAt, nowMs)}`}
				</span>
			</div>
			<button
				type="button"
				className={cn(panelIconBtn, "hover:text-failed")}
				onClick={onDelete}
				title={`Delete volume ${volume.name}`}
				aria-label={`Delete volume ${volume.name}`}
			>
				<Trash2 size={13} />
			</button>
		</li>
	);
}

type RetentionMode = "default" | "custom";

function LogRetentionForm({ project }: { project: DashboardProject }) {
	const router = useRouter();
	const saved = project.logRetentionDays ?? 0;
	const [mode, setMode] = useState<RetentionMode>(
		saved === 0 ? "default" : "custom",
	);
	const [days, setDays] = useState(saved === 0 ? "30" : String(saved));
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const [savedNotice, setSavedNotice] = useState(false);

	useEffect(() => {
		if (!savedNotice) return;
		const id = window.setTimeout(() => setSavedNotice(false), 2400);
		return () => window.clearTimeout(id);
	}, [savedNotice]);

	const parsedDays = /^\d+$/.test(days.trim())
		? Number(days.trim())
		: Number.NaN;
	const daysValid =
		Number.isInteger(parsedDays) && parsedDays >= 1 && parsedDays <= 90;
	const next = mode === "default" ? 0 : parsedDays;
	const dirty = mode === "default" ? saved !== 0 : parsedDays !== saved;
	const canSave = !saving && dirty && (mode === "default" || daysValid);

	const submit = async (event: FormEvent) => {
		event.preventDefault();
		if (!canSave) return;
		setSaving(true);
		setError(undefined);
		try {
			await doUpdateProjectLogRetention({
				data: { projectId: project.id, logRetentionDays: next },
			});
			setSavedNotice(true);
			await router.invalidate();
		} catch (cause) {
			// Keep the user's input so they can correct it.
			setError(formatError(cause));
		} finally {
			setSaving(false);
		}
	};

	return (
		<form className="flex flex-col gap-3.5" onSubmit={submit}>
			<fieldset className="m-0 flex flex-col gap-2 border-0 p-0">
				<legend className={cn(fieldLabel, "mb-2")}>Keep logs for</legend>
				<RetentionOption
					checked={mode === "default"}
					label="Platform default"
					detail="Follows the platform's retention policy."
					onSelect={() => setMode("default")}
				/>
				<RetentionOption
					checked={mode === "custom"}
					label="Custom"
					detail="Between 1 and 90 days."
					onSelect={() => setMode("custom")}
				>
					<div className="flex items-center gap-2">
						<input
							className={cn(fieldInput, "w-20 text-right")}
							inputMode="numeric"
							value={days}
							aria-label="Retention in days"
							aria-invalid={mode === "custom" && !daysValid}
							disabled={mode !== "custom"}
							onChange={(event) => {
								setDays(event.target.value);
								setError(undefined);
							}}
						/>
						<span className="text-[13px] text-muted">days</span>
					</div>
				</RetentionOption>
			</fieldset>

			{mode === "custom" && !daysValid && days.trim() !== "" && (
				<p className="m-0 text-xs text-failed">
					Enter a whole number of days from 1 to 90.
				</p>
			)}
			{error && (
				<p className={cn(errorMsg, "m-0")} role="alert">
					{error}
				</p>
			)}

			<div className="flex items-center gap-3">
				<button type="submit" className={btnPrimary} disabled={!canSave}>
					{saving && <Loader2 size={12} className="animate-spin" />}
					{saving ? "Saving…" : "Save retention"}
				</button>
				{savedNotice && (
					<output className="inline-flex items-center gap-1 text-xs text-healthy">
						<Check size={13} /> Saved
					</output>
				)}
			</div>
		</form>
	);
}

function RetentionOption({
	checked,
	label,
	detail,
	onSelect,
	children,
}: {
	checked: boolean;
	label: string;
	detail: string;
	onSelect: () => void;
	children?: ReactNode;
}) {
	return (
		<div
			className={cn(
				"flex items-center gap-3 border px-3.5 py-3 transition-colors duration-100 max-sm:flex-col max-sm:items-start",
				checked
					? "border-[rgba(226,138,36,0.5)] bg-accent-dim"
					: "border-line bg-canvas",
			)}
		>
			<label className="flex flex-1 cursor-pointer items-center gap-3">
				<input
					type="radio"
					name="log-retention-mode"
					className="size-3.5 accent-[var(--color-accent)]"
					checked={checked}
					onChange={onSelect}
				/>
				<span className="flex flex-col">
					<span className="text-[13px] font-semibold text-ink">{label}</span>
					<span className="text-xs text-muted">{detail}</span>
				</span>
			</label>
			{children}
		</div>
	);
}

function formatBytes(bytes: number): string {
	const units = ["B", "KiB", "MiB", "GiB", "TiB"];
	let value = bytes;
	let unit = 0;
	while (value >= 1024 && unit < units.length - 1) {
		value /= 1024;
		unit += 1;
	}
	return `${Number.isInteger(value) ? value : value.toFixed(1)} ${units[unit]}`;
}
