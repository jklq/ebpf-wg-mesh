import type { KeyboardEvent, MouseEvent, ReactNode } from "react";
import { createPortal } from "react-dom";

export function ModalOverlay({
	children,
	className,
	ariaLabel,
	onClose,
	closeOnBackdrop = true,
}: {
	children: ReactNode;
	className?: string;
	ariaLabel?: string;
	onClose?: () => void;
	closeOnBackdrop?: boolean;
}) {
	const overlay = (
		<div
			className={`modal-overlay${className ? ` ${className}` : ""}`}
			role="dialog"
			aria-modal="true"
			aria-label={ariaLabel}
			tabIndex={-1}
			onClick={(event: MouseEvent<HTMLDivElement>) => {
				if (closeOnBackdrop && event.target === event.currentTarget)
					onClose?.();
			}}
			onKeyDown={(event: KeyboardEvent<HTMLDivElement>) => {
				if (event.key !== "Escape") return;
				event.preventDefault();
				event.stopPropagation();
				onClose?.();
			}}
		>
			{children}
		</div>
	);

	// Service panels are transformed, so a fixed overlay rendered inside one
	// would otherwise be constrained to the panel rather than the viewport.
	return typeof document === "undefined"
		? overlay
		: createPortal(overlay, document.body);
}

export function InfoRow({
	label,
	children,
}: {
	label: string;
	children: ReactNode;
}) {
	return (
		<div className="info-row">
			<span className="info-row-label">{label}</span>
			<div
				style={{
					fontSize: 13,
					color: "var(--text)",
					overflow: "hidden",
					textOverflow: "ellipsis",
					whiteSpace: "nowrap",
					flex: 1,
				}}
			>
				{children}
			</div>
		</div>
	);
}

export function MonoValue({
	children,
	title,
}: {
	children: ReactNode;
	title?: string;
}) {
	return (
		<span
			style={{
				fontFamily: "var(--font-mono)",
				fontSize: 12,
				color: "var(--text)",
			}}
			title={title}
		>
			{children}
		</span>
	);
}
