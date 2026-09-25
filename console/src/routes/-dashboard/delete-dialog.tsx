import { AlertTriangle, History, Loader2, RefreshCw } from "lucide-react";
import {
	type FormEvent,
	type ReactNode,
	useCallback,
	useEffect,
	useRef,
	useState,
} from "react";

import { cn } from "#/lib/cn";
import type { DashboardDeletionPreview } from "#/lib/dashboard/core/types.server";
import {
	btnDangerSolid,
	btnGhost,
	btnSecondary,
	errorMsg,
	fieldInput,
	modalCard,
} from "#/lib/ui-classes";

import { formatError } from "./service-utils";
import { ModalOverlay } from "./ui";

export type DeleteDialogRecovery = "restorable" | "permanent";

/**
 * Confirms a tombstoning delete. When `loadPreview` is set the dialog lists
 * what goes with the resource before allowing the delete. `requireName`
 * demands the current name typed back (project, production environment,
 * volume). Errors from the delete keep the dialog open.
 */
export function DeleteDialog({
	title,
	name,
	description,
	recovery,
	requireName,
	loadPreview,
	previewServicesLabel = "Services",
	confirmLabel = "Delete",
	busy = false,
	error,
	onCancel,
	onConfirm,
}: {
	title: string;
	name: string;
	description: ReactNode;
	recovery: DeleteDialogRecovery;
	requireName: boolean;
	loadPreview?: () => Promise<DashboardDeletionPreview>;
	previewServicesLabel?: string;
	confirmLabel?: string;
	busy?: boolean;
	error?: string;
	onCancel: () => void;
	onConfirm: (confirmationName: string) => void;
}) {
	const [typed, setTyped] = useState("");
	const [preview, setPreview] = useState<DashboardDeletionPreview>();
	const [previewError, setPreviewError] = useState<string>();
	const [previewLoading, setPreviewLoading] = useState(Boolean(loadPreview));
	const inputRef = useRef<HTMLInputElement>(null);
	const cancelRef = useRef<HTMLButtonElement>(null);
	const loadPreviewRef = useRef(loadPreview);
	loadPreviewRef.current = loadPreview;
	const matches = !requireName || typed.trim() === name;
	const previewReady = !loadPreview || Boolean(preview);

	const fetchPreview = useCallback(async () => {
		const load = loadPreviewRef.current;
		if (!load) return;
		setPreviewLoading(true);
		setPreviewError(undefined);
		try {
			setPreview(await load());
		} catch (cause) {
			setPreviewError(formatError(cause));
		} finally {
			setPreviewLoading(false);
		}
	}, []);

	useEffect(() => {
		void fetchPreview();
	}, [fetchPreview]);

	useEffect(() => {
		(requireName ? inputRef.current : cancelRef.current)?.focus();
	}, [requireName]);

	const submit = (event: FormEvent) => {
		event.preventDefault();
		if (!matches || !previewReady || busy) return;
		onConfirm(requireName ? typed.trim() : "");
	};

	return (
		<ModalOverlay
			className="items-center p-6"
			ariaLabel={title}
			onClose={() => {
				if (!busy) onCancel();
			}}
		>
			<form
				className={cn(
					modalCard,
					"flex max-w-[480px] flex-col gap-3.5 border-[rgba(184,66,66,0.55)] p-[18px] shadow-[0_24px_70px_rgba(0,0,0,0.6)] select-text",
				)}
				onSubmit={submit}
			>
				<div className="flex items-center gap-2.5">
					<AlertTriangle size={20} className="shrink-0 text-failed" />
					<h2 className="m-0 font-condensed text-[21px] tracking-[0.02em] text-ink">
						{title}
					</h2>
				</div>

				<p className="m-0 text-[13px] leading-relaxed text-muted select-text">
					{description}
				</p>

				{loadPreview && (
					<DeletionPreviewList
						preview={preview}
						loading={previewLoading}
						error={previewError}
						servicesLabel={previewServicesLabel}
						onRetry={() => void fetchPreview()}
					/>
				)}

				<p
					className={cn(
						"m-0 flex items-start gap-2 text-xs leading-normal",
						recovery === "permanent" ? "text-failed" : "text-muted",
					)}
				>
					{recovery === "permanent" ? (
						<AlertTriangle size={13} className="mt-px shrink-0" />
					) : (
						<History size={13} className="mt-px shrink-0" />
					)}
					<span>
						{recovery === "permanent"
							? "This cannot be restored. The data is destroyed when the grace period ends."
							: "You can restore it from Recently deleted until the grace period ends."}
					</span>
				</p>

				{requireName && (
					<>
						<p className="m-0 text-[13px] leading-relaxed text-muted select-text">
							Type{" "}
							<strong className="font-mono font-semibold text-ink">
								{name}
							</strong>{" "}
							to confirm
						</p>
						<input
							ref={inputRef}
							className={cn(
								fieldInput,
								"font-mono focus:!border-failed focus:!shadow-[0_0_0_2px_var(--color-failed-dim)]",
							)}
							value={typed}
							onChange={(event) => setTyped(event.target.value)}
							placeholder={name}
							autoComplete="off"
							spellCheck={false}
							aria-label={`Type ${name} to confirm`}
						/>
					</>
				)}

				{error && (
					<p className={cn(errorMsg, "m-0")} role="alert">
						{error}
					</p>
				)}

				<div className="mt-0.5 flex justify-end gap-2">
					<button
						ref={cancelRef}
						type="button"
						className={btnSecondary}
						onClick={onCancel}
						disabled={busy}
					>
						Cancel
					</button>
					<button
						type="submit"
						className={btnDangerSolid}
						disabled={!matches || !previewReady || busy}
					>
						{busy && <Loader2 size={13} className="animate-spin" />}
						{busy ? "Deleting…" : confirmLabel}
					</button>
				</div>
			</form>
		</ModalOverlay>
	);
}

function DeletionPreviewList({
	preview,
	loading,
	error,
	servicesLabel,
	onRetry,
}: {
	preview: DashboardDeletionPreview | undefined;
	loading: boolean;
	error?: string;
	servicesLabel: string;
	onRetry: () => void;
}) {
	if (error) {
		return (
			<div className="flex items-center justify-between gap-3 border border-line bg-canvas px-3 py-2.5 text-xs text-failed">
				<span>Could not load what this removes: {error}</span>
				<button type="button" className={btnGhost} onClick={onRetry}>
					<RefreshCw size={12} />
					Retry
				</button>
			</div>
		);
	}
	if (loading || !preview) {
		return (
			<div
				className="flex items-center gap-2 border border-line bg-canvas px-3 py-2.5 text-xs text-muted"
				aria-busy="true"
			>
				<Loader2 size={12} className="animate-spin" />
				Checking what goes with it…
			</div>
		);
	}
	const groups = [
		{
			label: "Environments",
			items: preview.environments.map((entry) => ({
				key: entry.id,
				text: entry.name,
				tag: entry.isProduction ? "prod" : undefined,
			})),
		},
		{
			label: servicesLabel,
			items: preview.services.map((entry) => ({
				key: entry.id,
				text: entry.name,
				tag: entry.environmentName || undefined,
			})),
		},
		{
			label: "Domains",
			items: preview.domains.map((entry) => ({
				key: entry.hostname,
				text: entry.hostname,
				tag: undefined,
			})),
		},
		{
			label: "Volumes",
			items: preview.volumes.map((entry) => ({
				key: entry.id,
				text: entry.name,
				tag: undefined,
			})),
		},
	].filter((group) => group.items.length > 0);

	if (groups.length === 0) {
		return (
			<div className="border border-line bg-canvas px-3 py-2.5 text-xs text-muted">
				Nothing else goes with it.
			</div>
		);
	}

	return (
		<section
			className="flex max-h-[40vh] flex-col gap-2.5 overflow-y-auto border border-line bg-canvas px-3 py-3"
			aria-label="What goes with it"
		>
			<span className="font-condensed text-[10px] font-bold tracking-[0.1em] text-muted uppercase">
				Also removed
			</span>
			{groups.map((group) => (
				<div key={group.label} className="flex flex-col gap-1.5">
					<span className="text-[11px] text-dim">
						{group.label}{" "}
						<span className="font-mono text-muted">{group.items.length}</span>
					</span>
					<ul className="m-0 flex list-none flex-wrap gap-1.5 p-0">
						{group.items.map((item) => (
							<li
								key={item.key}
								className="inline-flex max-w-full items-center gap-1.5 border border-line-bright bg-surface-raised px-1.5 py-0.5 font-mono text-[11px] text-ink"
							>
								<span className="truncate" title={item.text}>
									{item.text}
								</span>
								{item.tag && (
									<span className="shrink-0 text-[10px] text-dim">
										{item.tag}
									</span>
								)}
							</li>
						))}
					</ul>
				</div>
			))}
		</section>
	);
}
