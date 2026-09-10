import { Loader2, X } from "lucide-react";

import { cn } from "#/lib/cn";
import { btnGhost, modalCard } from "#/lib/ui-classes";

import { ModalOverlay } from "./ui";

export function NewServiceModalFallback({ onClose }: { onClose: () => void }) {
	return (
		<ModalOverlay onClose={onClose} ariaLabel="Deploy service">
			<div className={modalCard}>
				<div className="flex items-center gap-1.5 border-b border-line px-3">
					<span className="flex flex-1 items-center gap-2 py-2.5 text-[13px] text-muted">
						<Loader2 size={13} className="shrink-0 animate-spin" />
						Loading repositories…
					</span>
					<button
						type="button"
						className={cn(btnGhost, "px-1.5 py-1")}
						onClick={onClose}
						title="Close deploy dialog"
					>
						<X size={14} />
					</button>
				</div>
			</div>
		</ModalOverlay>
	);
}
