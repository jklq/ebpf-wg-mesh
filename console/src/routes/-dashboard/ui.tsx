import type { ReactNode } from "react";

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
