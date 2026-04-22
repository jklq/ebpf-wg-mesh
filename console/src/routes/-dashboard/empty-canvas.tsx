import { Layers, Plus } from "lucide-react";

export function EmptyCanvas({ onAdd }: { onAdd: () => void }) {
	return (
		<div className="empty-canvas">
			<div
				style={{
					width: 56,
					height: 56,
					borderRadius: 12,
					background: "var(--surface-raised)",
					border: "1px solid var(--border)",
					display: "flex",
					alignItems: "center",
					justifyContent: "center",
				}}
			>
				<Layers size={24} color="var(--text-dim)" />
			</div>
			<div style={{ textAlign: "center" }}>
				<p
					style={{
						margin: "0 0 4px",
						fontSize: 14,
						fontWeight: 600,
						color: "var(--text)",
					}}
				>
					No services
				</p>
				<p style={{ margin: 0, fontSize: 12, color: "var(--text-muted)" }}>
					Deploy your first service from a GitHub repo
				</p>
			</div>
			<button
				type="button"
				className="btn-primary"
				onMouseDown={(event) => event.stopPropagation()}
				onClick={(event) => {
					event.stopPropagation();
					onAdd();
				}}
			>
				<Plus size={14} />
				Deploy service
			</button>
		</div>
	);
}
