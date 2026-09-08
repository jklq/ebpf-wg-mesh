import type { KeyboardEvent, MouseEvent, ReactNode } from "react";
import { createPortal } from "react-dom";

import { cn } from "#/lib/cn";
import { modalOverlay } from "#/lib/ui-classes";

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
			className={cn(modalOverlay, className)}
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

export function PanelSection({
	title,
	lede,
	tone = "default",
	children,
}: {
	title: string;
	lede?: ReactNode;
	tone?: "default" | "danger";
	children: ReactNode;
}) {
	return (
		<section className="relative">
			<header className="mb-4 border-b border-white/5 pb-3">
				<div className="min-w-0">
					<h3
						className={cn(
							"m-0 font-display text-[22px] font-medium leading-[1.1] tracking-tight text-ink",
							tone === "danger" && "text-failed",
						)}
					>
						{title}
					</h3>
					{lede ? (
						<p className="mt-[5px] mb-0 text-[13px] leading-[1.45] text-muted">
							{lede}
						</p>
					) : null}
				</div>
			</header>
			<div className="flex flex-col gap-3.5">{children}</div>
		</section>
	);
}
