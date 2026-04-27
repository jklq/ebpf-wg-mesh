import {
	AlertCircle,
	CheckCircle2,
	Clock,
	Loader2,
	Server,
	Terminal,
} from "lucide-react";
import type { MouseEvent as ReactMouseEvent } from "react";

import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";

import { NODE_H, NODE_W } from "./layout";
import { healthLabel, serviceHealth, shortSha } from "./service-utils";

export function ServiceNode({
	service,
	pos,
	selected,
	onMouseDown,
	onSelect,
}: {
	service: DashboardServiceRecord;
	pos: { x: number; y: number };
	selected: boolean;
	onMouseDown: (event: ReactMouseEvent<HTMLElement>) => void;
	onSelect: () => void;
}) {
	const health = serviceHealth(service);
	const source = service.spec?.source;
	const repo = source?.repositorySelector ?? "";
	const repoShort = repo.split("/").pop() ?? repo;
	const unappliedCount =
		service.unappliedChangeCount ?? (service.pendingChanges ? 1 : 0);

	return (
		<button
			type="button"
			className={`service-node ${selected ? "selected" : ""}`}
			style={{
				left: pos.x,
				top: pos.y,
				width: NODE_W,
				height: NODE_H,
				boxSizing: "border-box",
				padding: 0,
				color: "inherit",
				textAlign: "left",
				overflow: "hidden",
			}}
			aria-pressed={selected}
			onMouseDown={onMouseDown}
			onClick={onSelect}
		>
			<div
				style={{
					padding: "10px 14px 8px",
					borderBottom: "1px solid var(--border)",
					display: "flex",
					alignItems: "center",
					gap: 8,
				}}
			>
				<span className={`status-dot ${health}`} style={{ flexShrink: 0 }} />
				<span
					style={{
						fontSize: 14,
						fontWeight: 700,
						letterSpacing: "0.03em",
						fontFamily: "'Barlow Condensed', sans-serif",
						color: "var(--text)",
						overflow: "hidden",
						textOverflow: "ellipsis",
						whiteSpace: "nowrap",
						flex: 1,
					}}
				>
					{service.name}
				</span>
			</div>

			<div
				style={{
					padding: "8px 14px 10px",
					display: "flex",
					flexDirection: "column",
					gap: 5,
				}}
			>
				{repoShort && (
					<div
						style={{
							display: "flex",
							alignItems: "center",
							gap: 5,
							fontSize: 11,
							color: "var(--text-muted)",
							fontFamily: "var(--font-mono)",
							overflow: "hidden",
							textOverflow: "ellipsis",
							whiteSpace: "nowrap",
						}}
					>
						<Server size={10} style={{ flexShrink: 0 }} />
						{repoShort}
					</div>
				)}

				{source?.trackedRef && (
					<div
						style={{
							display: "flex",
							alignItems: "center",
							gap: 5,
							fontSize: 11,
							color: "var(--text-dim)",
							fontFamily: "var(--font-mono)",
						}}
					>
						<Terminal size={10} style={{ flexShrink: 0 }} />
						{source.trackedRef}
					</div>
				)}

				<div
					style={{
						display: "flex",
						alignItems: "center",
						justifyContent: "space-between",
						gap: 6,
						marginTop: 2,
					}}
				>
					<div style={{ display: "flex", alignItems: "center", gap: 5 }}>
						<span className={`badge ${health}`}>
							{health === "building" && (
								<Loader2
									size={10}
									style={{ animation: "spin 1s linear infinite" }}
								/>
							)}
							{health === "healthy" && <CheckCircle2 size={10} />}
							{health === "failed" && <AlertCircle size={10} />}
							{health === "offline" && <Clock size={10} />}
							{healthLabel(health)}
						</span>
						{unappliedCount > 0 && (
							<span className="badge edited">
								Edited · {unappliedCount}{" "}
								{unappliedCount === 1 ? "change" : "changes"}
							</span>
						)}
					</div>

					{service.latestBuild?.commitSha && (
						<span
							style={{
								fontSize: 10,
								fontFamily: "var(--font-mono)",
								color: "var(--text-dim)",
							}}
						>
							{shortSha(service.latestBuild.commitSha)}
						</span>
					)}
				</div>
			</div>
		</button>
	);
}
