import { AlertTriangle, Loader2 } from "lucide-react";
import {
	type FormEvent,
	type ReactNode,
	useEffect,
	useRef,
	useState,
} from "react";

import { ModalOverlay } from "./ui";

export function ConfirmDeleteDialog({
	title,
	name,
	description,
	confirmLabel = "Delete",
	busy = false,
	error,
	onCancel,
	onConfirm,
}: {
	title: string;
	name: string;
	description: ReactNode;
	confirmLabel?: string;
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
			className="danger-overlay"
			ariaLabel={title}
			onClose={() => {
				if (!busy) onCancel();
			}}
		>
			<form className="modal-card danger-card" onSubmit={submit}>
				<div className="danger-card-head">
					<AlertTriangle size={20} className="danger-card-icon" />
					<h2>{title}</h2>
				</div>

				<p className="danger-card-body">{description}</p>

				<p className="danger-card-body">
					Type <strong className="danger-card-name">{name}</strong> to confirm
				</p>

				<input
					ref={inputRef}
					className="field-input danger-card-input"
					value={typed}
					onChange={(event) => setTyped(event.target.value)}
					placeholder={name}
					autoComplete="off"
					spellCheck={false}
					aria-label={`Type ${name} to confirm`}
				/>

				{error && <p className="error-msg danger-card-error">{error}</p>}

				<div className="danger-card-actions">
					<button
						type="button"
						className="btn-secondary"
						onClick={onCancel}
						disabled={busy}
					>
						Cancel
					</button>
					<button
						type="submit"
						className="btn-danger-solid"
						disabled={!matches || busy}
					>
						{busy && (
							<Loader2
								size={13}
								style={{ animation: "spin 1s linear infinite" }}
							/>
						)}
						{busy ? "Deleting…" : confirmLabel}
					</button>
				</div>
			</form>
		</ModalOverlay>
	);
}
