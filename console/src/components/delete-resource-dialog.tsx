import * as stylex from "@stylexjs/stylex";
import { Button } from "#/components/ui/button";
import { dialogStyles } from "#/components/ui/dialog";
import { TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

import { AlertTriangle, History, Loader2, RefreshCw } from "lucide-react";
import {
	type FormEvent,
	type ReactNode,
	useCallback,
	useEffect,
	useRef,
	useState,
} from "react";
import { Dialog } from "#/components/ui/dialog";
import type { DashboardDeletionPreview } from "#/lib/dashboard/core/types.server";
import { formatError } from "#/lib/errors";

export type DeleteResourceDialogRecovery = "restorable" | "permanent";

/** Confirms a tombstoning delete, optionally previewing what goes with the resource. */
export function DeleteResourceDialog({
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
	recovery: DeleteResourceDialogRecovery;
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
		<Dialog
			styles={styles.overlay}
			label={title}
			onClose={() => {
				if (!busy) onCancel();
			}}
		>
			<form
				{...stylex.props([dialogStyles.card, styles.form])}
				onSubmit={submit}
			>
				<div {...stylex.props(styles.header)}>
					<AlertTriangle size={20} {...stylex.props(styles.warningIcon)} />
					<h2 {...stylex.props(styles.title)}>{title}</h2>
				</div>

				<p {...stylex.props(styles.description)}>{description}</p>

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
					{...stylex.props([
						styles.recoveryNotice,
						recovery === "permanent"
							? styles.permanentRecovery
							: styles.restorableRecovery,
					])}
				>
					{recovery === "permanent" ? (
						<AlertTriangle size={13} {...stylex.props(styles.noticeIcon)} />
					) : (
						<History size={13} {...stylex.props(styles.noticeIcon)} />
					)}
					<span>
						{recovery === "permanent"
							? "This cannot be restored. The data is destroyed when the grace period ends."
							: "You can restore it from Recently deleted until the grace period ends."}
					</span>
				</p>

				{requireName && (
					<>
						<p {...stylex.props(styles.description)}>
							Type{" "}
							<strong {...stylex.props(styles.resourceName)}>{name}</strong> to
							confirm
						</p>
						<TextInput
							ref={inputRef}
							styles={[styles.confirmationInput]}
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
					<p
						{...stylex.props([noticeStyles.error, styles.errorMessage])}
						role="alert"
					>
						{error}
					</p>
				)}

				<div {...stylex.props(styles.actions)}>
					<Button
						ref={cancelRef}
						type="button"
						variant="secondary"
						onClick={onCancel}
						disabled={busy}
					>
						Cancel
					</Button>
					<Button
						type="submit"
						variant="dangerSolid"
						disabled={!matches || !previewReady || busy}
					>
						{busy && <Loader2 size={13} {...stylex.props(styles.spinner)} />}
						{busy ? "Deleting…" : confirmLabel}
					</Button>
				</div>
			</form>
		</Dialog>
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
			<div {...stylex.props(styles.previewError)}>
				<span>Could not load what this removes: {error}</span>
				<Button type="button" variant="ghost" onClick={onRetry}>
					<RefreshCw size={12} />
					Retry
				</Button>
			</div>
		);
	}
	if (loading || !preview) {
		return (
			<div {...stylex.props(styles.previewLoading)} aria-busy="true">
				<Loader2 size={12} {...stylex.props(styles.spinner)} />
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
			<div {...stylex.props(styles.previewEmpty)}>
				Nothing else goes with it.
			</div>
		);
	}

	return (
		<section {...stylex.props(styles.preview)} aria-label="What goes with it">
			<span {...stylex.props(styles.previewTitle)}>Also removed</span>
			{groups.map((group) => (
				<div key={group.label} {...stylex.props(styles.previewGroup)}>
					<span {...stylex.props(styles.groupTitle)}>
						{group.label}{" "}
						<span {...stylex.props(styles.groupCount)}>
							{group.items.length}
						</span>
					</span>
					<ul {...stylex.props(styles.resourceList)}>
						{group.items.map((item) => (
							<li key={item.key} {...stylex.props(styles.resourceRow)}>
								<span {...stylex.props(styles.resourceLabel)} title={item.text}>
									{item.text}
								</span>
								{item.tag && (
									<span {...stylex.props(styles.resourceTag)}>{item.tag}</span>
								)}
							</li>
						))}
					</ul>
				</div>
			))}
		</section>
	);
}

const styles = stylex.create({
	overlay: { alignItems: "center", padding: space.xl },
	form: {
		display: "flex",
		maxWidth: "480px",
		flexDirection: "column",
		gap: "0.875rem",
		borderColor: "rgba(184,66,66,0.55)",
		padding: "18px",
		boxShadow: "0 24px 70px rgba(0,0,0,0.6)",
		WebkitUserSelect: "text",
		userSelect: "text",
	},
	header: { display: "flex", alignItems: "center", gap: "0.625rem" },
	warningIcon: { flexShrink: "0", color: colors.failed },
	title: {
		margin: "0rem",
		fontFamily: fonts.condensed,
		fontSize: "21px",
		letterSpacing: "0.02em",
		color: colors.ink,
	},
	description: {
		margin: "0rem",
		fontSize: "13px",
		lineHeight: "1.625",
		color: colors.muted,
		WebkitUserSelect: "text",
		userSelect: "text",
	},
	recoveryNotice: {
		margin: "0rem",
		display: "flex",
		alignItems: "flex-start",
		gap: space.sm,
		fontSize: "0.75rem",
		lineHeight: "1.5",
	},
	permanentRecovery: { color: colors.failed },
	restorableRecovery: { color: colors.muted },
	noticeIcon: { marginTop: "1px", flexShrink: "0" },
	resourceName: {
		fontFamily: fonts.mono,
		fontWeight: "600",
		color: colors.ink,
	},
	confirmationInput: {
		fontFamily: fonts.mono,
		borderColor: { default: null, ":focus": colors.failed },
		boxShadow: { default: null, ":focus": `0 0 0 2px ${colors.failedDim}` },
	},
	errorMessage: { margin: "0rem" },
	actions: {
		marginTop: "0.125rem",
		display: "flex",
		justifyContent: "flex-end",
		gap: space.sm,
	},
	spinner: { animation: `${spin} 1s linear infinite` },
	previewError: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.failed,
	},
	previewLoading: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	previewEmpty: {
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	preview: {
		display: "flex",
		maxHeight: "40vh",
		flexDirection: "column",
		gap: "0.625rem",
		overflowY: "auto",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
		paddingInline: space.md,
		paddingBlock: space.md,
	},
	previewTitle: {
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		letterSpacing: "0.1em",
		color: colors.muted,
		textTransform: "uppercase",
	},
	previewGroup: { display: "flex", flexDirection: "column", gap: "0.375rem" },
	groupTitle: { fontSize: "11px", color: colors.dim },
	groupCount: { fontFamily: fonts.mono, color: colors.muted },
	resourceList: {
		margin: "0rem",
		display: "flex",
		listStyleType: "none",
		flexWrap: "wrap",
		gap: "0.375rem",
		padding: "0rem",
	},
	resourceRow: {
		display: "inline-flex",
		maxWidth: "100%",
		alignItems: "center",
		gap: "0.375rem",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: colors.surfaceRaised,
		paddingInline: "0.375rem",
		paddingBlock: "0.125rem",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.ink,
	},
	resourceLabel: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	resourceTag: { flexShrink: "0", fontSize: "10px", color: colors.dim },
});
