import * as stylex from "@stylexjs/stylex";
import { Check, Loader2, X } from "lucide-react";
import { type FormEvent, useEffect, useRef, useState } from "react";
import type {
	DashboardProject,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";
import { doUpdateService } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, motion } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
const styles = stylex.create({
	spinner: { animation: `${spin} 1s linear infinite` },
	renameForm: {
		display: "flex",
		height: "2rem",
		minWidth: "0rem",
		maxWidth: "min(520px, calc(100% - 72px))",
		flex: "1",
		alignItems: "center",
		gap: "0rem",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "rgba(92,170,112,0.42)",
		backgroundImage:
			"linear-gradient(180deg,rgba(255,255,255,0.035),transparent)",
		backgroundColor: "rgba(15,14,13,0.86)",
		padding: "0.125rem",
		boxShadow:
			"inset 0 -1px 0 rgba(92,170,112,0.2), 0 0 0 1px rgba(15,14,13,0.8)",
	},
	nameInput: {
		height: "100%",
		minWidth: "120px",
		flex: "1",
		appearance: "none",
		borderRadius: "0",
		borderStyle: "solid",
		borderWidth: "0px",
		backgroundColor: {
			default: "transparent",
			"::selection": "rgba(92,170,112,0.28)",
		},
		paddingInline: "9px",
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
		letterSpacing: "-0.03em",
		color: colors.ink,
		caretColor: colors.accent,
		boxShadow: "none",
		outlineStyle: "none",
	},
	saveButton: {
		display: "inline-flex",
		height: "100%",
		width: "30px",
		cursor: { default: "pointer", ":disabled": "not-allowed" },
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "0px",
		borderLeftStyle: "solid",
		borderLeftWidth: "1px",
		borderColor: "rgba(80,76,71,0.65)",
		backgroundColor: {
			default: "transparent",
			":enabled": {
				default: null,
				":hover": {
					default: null,
					"@media (hover: hover)":
						"color-mix(in oklab, #fff 4.5%, transparent)",
				},
				":focus-visible": "color-mix(in oklab, #fff 4.5%, transparent)",
			},
		},
		color: {
			default: colors.muted,
			":enabled": {
				default: null,
				":hover": { default: null, "@media (hover: hover)": colors.accent },
				":focus-visible": colors.accent,
			},
		},
		transitionProperty: "color,background-color",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
		outlineStyle: {
			default: null,
			":enabled": { default: null, ":focus-visible": "none" },
		},
		opacity: { default: null, ":disabled": "40%" },
	},
	cancelButton: {
		display: "inline-flex",
		height: "100%",
		width: "30px",
		cursor: { default: "pointer", ":disabled": "not-allowed" },
		alignItems: "center",
		justifyContent: "center",
		borderStyle: "solid",
		borderWidth: "0px",
		borderLeftStyle: "solid",
		borderLeftWidth: "1px",
		borderColor: "rgba(80,76,71,0.65)",
		backgroundColor: {
			default: "transparent",
			":enabled": {
				default: null,
				":hover": {
					default: null,
					"@media (hover: hover)":
						"color-mix(in oklab, #fff 4.5%, transparent)",
				},
				":focus-visible": "color-mix(in oklab, #fff 4.5%, transparent)",
			},
		},
		color: {
			default: colors.muted,
			":enabled": {
				default: null,
				":hover": { default: null, "@media (hover: hover)": colors.failed },
				":focus-visible": colors.failed,
			},
		},
		transitionProperty: "color,background-color",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
		outlineStyle: {
			default: null,
			":enabled": { default: null, ":focus-visible": "none" },
		},
		opacity: { default: null, ":disabled": "40%" },
	},
	errorMessage: {
		minWidth: "0rem",
		overflow: "hidden",
		fontSize: "11px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.failed,
	},
	renameButton: {
		display: "inline-flex",
		height: "1.75rem",
		maxWidth: "min(420px, calc(100% - 72px))",
		minWidth: "0rem",
		cursor: { default: "pointer", ":disabled": "default" },
		alignItems: "center",
		overflow: "hidden",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: "transparent",
		borderBottomColor: {
			default: "rgba(255,255,255,0.12)",
			":enabled": {
				default: null,
				":hover": { default: null, "@media (hover: hover)": colors.accent },
				":focus-visible": colors.accent,
			},
		},
		backgroundColor: {
			default: "transparent",
			":enabled": {
				default: null,
				":hover": {
					default: null,
					"@media (hover: hover)":
						"color-mix(in oklab, #fff 2.5%, transparent)",
				},
				":focus-visible": "color-mix(in oklab, #fff 2.5%, transparent)",
			},
		},
		padding: "0rem",
		color: colors.ink,
		outlineStyle: {
			default: null,
			":enabled": { default: null, ":focus-visible": "none" },
		},
	},
	serviceName: {
		maxWidth: "100%",
		minWidth: "0rem",
		overflow: "hidden",
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
		letterSpacing: "-0.03em",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
});
export function EditableServiceHeaderName({
	service,
	project,
	onSaved,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject | undefined;
	onSaved: (service: DashboardServiceRecord) => void;
}) {
	const [editing, setEditing] = useState(false);
	const [draftName, setDraftName] = useState(service.name);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState<string>();
	const inputRef = useRef<HTMLInputElement>(null);
	const formRef = useRef<HTMLFormElement>(null);

	useEffect(() => {
		if (!editing) {
			setDraftName(service.name);
			setError(undefined);
		}
	}, [editing, service.name]);

	useEffect(() => {
		if (editing) {
			inputRef.current?.focus();
			inputRef.current?.select();
		}
	}, [editing]);

	useEffect(() => {
		if (!editing) return;
		const onPointerDown = (event: MouseEvent) => {
			const root = formRef.current;
			if (!root || root.contains(event.target as Node)) return;
			setDraftName(service.name);
			setError(undefined);
			setEditing(false);
		};
		document.addEventListener("mousedown", onPointerDown);
		return () => document.removeEventListener("mousedown", onPointerDown);
	}, [editing, service.name]);

	const startEditing = () => {
		setDraftName(service.name);
		setError(undefined);
		setEditing(true);
	};

	const cancelEditing = () => {
		setDraftName(service.name);
		setError(undefined);
		setEditing(false);
	};

	const saveName = async (event: FormEvent) => {
		event.preventDefault();
		if (!project || saving) return;
		const nextName = draftName.trim();
		if (!nextName || nextName === service.name) {
			cancelEditing();
			return;
		}
		const previousService = service;
		const optimisticService: DashboardServiceRecord = {
			...service,
			name: nextName,
			updatedAt: new Date().toISOString(),
		};
		setSaving(true);
		setError(undefined);
		setEditing(false);
		onSaved(optimisticService);
		try {
			const updated = await doUpdateService({
				data: {
					serviceId: service.id,
					serviceName: nextName,
				},
			});
			onSaved(updated);
		} catch (e) {
			onSaved(previousService);
			setDraftName(nextName);
			setError(formatError(e));
			setEditing(true);
		} finally {
			setSaving(false);
		}
	};

	if (editing) {
		return (
			<form
				ref={formRef}
				{...stylex.props(styles.renameForm)}
				onSubmit={saveName}
			>
				<input
					ref={inputRef}
					{...stylex.props(styles.nameInput)}
					value={draftName}
					onChange={(event) => setDraftName(event.target.value)}
					onKeyDown={(event) => {
						if (event.key === "Escape") {
							event.preventDefault();
							cancelEditing();
						}
					}}
					aria-label="Service name"
				/>
				<button
					type="submit"
					{...stylex.props(styles.saveButton)}
					disabled={saving || draftName.trim() === ""}
					title="Save service name"
				>
					{saving ? (
						<Loader2 size={13} {...stylex.props(styles.spinner)} />
					) : (
						<Check size={14} />
					)}
				</button>
				<button
					type="button"
					{...stylex.props(styles.cancelButton)}
					onClick={cancelEditing}
					disabled={saving}
					title="Cancel service name edit"
				>
					<X size={14} />
				</button>
				{error && <span {...stylex.props(styles.errorMessage)}>{error}</span>}
			</form>
		);
	}

	return (
		<button
			type="button"
			{...stylex.props(styles.renameButton)}
			onClick={startEditing}
			disabled={!project}
			title="Rename service"
		>
			<span {...stylex.props(styles.serviceName)}>{service.name}</span>
		</button>
	);
}
