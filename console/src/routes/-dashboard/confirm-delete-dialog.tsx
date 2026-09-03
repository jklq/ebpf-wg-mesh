import { AlertTriangle, Loader2 } from "lucide-react";
import {
	type FormEvent,
	type ReactNode,
	useEffect,
	useRef,
	useState,
} from "react";

import { cn } from "#/lib/cn";
import {
	btnDangerSolid,
	btnSecondary,
	errorMsg,
	fieldInput,
	modalCard,
} from "#/lib/ui-classes";

import { ModalOverlay } from "./ui";

export function ConfirmDeleteDialog({
	title,
	name,
	description,
	confirmLabel = "Delete",
	busyLabel,
	busy = false,
	error,
	onCancel,
	onConfirm,
}: {
	title: string;
	name: string;
	description: ReactNode;
	confirmLabel?: string;
	busyLabel?: string;
	busy?: boolean;
	error?: string;
	onCancel: () => void;
	onConfirm: () => void;
}) {
	const [typed, setTyped] = useState("");
	const inputRef = useRef<HTMLInputElement>(null);
	const matches = typed.trim() === name;

	useEffect(() => {
		inputRef.current?.focus();
	}, []);

	const submit = (event: FormEvent) => {
		event.preventDefault();
		if (!matches || busy) return;
		onConfirm();
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
					"flex max-w-[460px] flex-col gap-3 border-[rgba(184,66,66,0.55)] p-[18px] shadow-[0_24px_70px_rgba(0,0,0,0.6)] select-text",
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

				<p className="m-0 text-[13px] leading-relaxed text-muted select-text">
					Type{" "}
					<strong className="font-mono font-semibold text-ink">{name}</strong>{" "}
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

				{error && <p className={cn(errorMsg, "m-0")}>{error}</p>}

				<div className="mt-0.5 flex justify-end gap-2">
					<button
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
						disabled={!matches || busy}
					>
						{busy && <Loader2 size={13} className="animate-spin" />}
						{busy ? (busyLabel ?? "Deleting…") : confirmLabel}
					</button>
				</div>
			</form>
		</ModalOverlay>
	);
}
