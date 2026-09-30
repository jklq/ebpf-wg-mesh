import * as stylex from "@stylexjs/stylex";
import { useEffect, useRef, useState } from "react";
import { deploymentStagesForDisplay } from "#/features/dashboard/service-panel/deployments/deployment-inline";
import { DeploymentProgress } from "#/features/dashboard/service-panel/deployments/deployment-progress";
import { withSourceStage } from "#/features/dashboard/service-panel/deployments/panel-deployments-helpers";
import {
	serviceHealth,
	serviceStatusLabel,
} from "#/features/dashboard/shared/service-utils";
import { usePulseDelay } from "#/hooks/use-pulse-delay";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const panelBadgeExit = stylex.keyframes({
	to: { opacity: "0", transform: "scale(0.9) translateX(3px)" },
});
const panelBadgeEnter = stylex.keyframes({
	from: { opacity: "0", transform: "translateX(5px)" },
});
const heroGlimmer = stylex.keyframes({
	from: { opacity: "0", transform: "translateX(-150%)" },
	"15%": { opacity: "1" },
	"85%": { opacity: "1" },
	to: { opacity: "0", transform: "translateX(150%)" },
});

const styles = stylex.create({
	statusBadge: {
		position: "relative",
		display: "flex",
		height: "26px",
		minWidth: "6rem",
		flexShrink: "0",
		alignItems: "center",
		gap: "0.375rem",
		overflow: "hidden",
		borderStyle: "solid",
		borderWidth: "1px",
		paddingInline: space.sm,
	},
	statusExiting: {
		animation: `${panelBadgeExit} 0.2s cubic-bezier(0.4, 0, 1, 1) both`,
	},
	statusEntering: {
		animation: `${panelBadgeEnter} 0.22s cubic-bezier(0.25, 0.46, 0.45, 0.94) 0.18s both`,
	},
	buildingBadge: {
		borderColor: "rgba(192,133,32,0.28)",
		backgroundColor: "rgba(192,133,32,0.07)",
	},
	failedBadge: {
		borderColor: "rgba(184,66,66,0.26)",
		backgroundColor: "rgba(184,66,66,0.06)",
	},
	neutralBadge: {
		borderColor: colors.line,
		backgroundColor: "color-mix(in oklab, #fff 2.5%, transparent)",
	},
	completionGlimmer: {
		pointerEvents: "none",
		position: "absolute",
		inset: "0rem",
		animation: `${heroGlimmer} 0.38s ease-in-out both`,
		backgroundImage:
			"linear-gradient(to right in oklab, transparent 0%, color-mix(in srgb, #fff 14%, transparent) 50%, transparent 100%)",
	},
	statusLabel: {
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		letterSpacing: "0.09em",
		whiteSpace: "nowrap",
		textTransform: "uppercase",
	},
	buildingLabel: { color: colors.building },
	failedLabel: { color: colors.failed },
	healthyLabel: { color: colors.healthy },
	neutralLabel: { color: colors.muted },
});
export function ServiceStatusBadge({
	service,
}: {
	service: DashboardServiceRecord;
}) {
	const currentService = service;
	const build = currentService.latestBuild ?? service.latestBuild;
	const health = serviceHealth(currentService);
	const hasUndeployedChanges =
		(currentService.unappliedChangeCount ??
			(currentService.pendingChanges ? 1 : 0)) > 0;
	const stages = withSourceStage(
		currentService,
		build,
		deploymentStagesForDisplay(
			currentService.latestDeployment,
			build,
			build?.stages ?? [],
		),
	);
	const prevHealthRef = useRef(health);
	const [heroVisible, setHeroVisible] = useState(health !== "healthy");
	const [heroExiting, setHeroExiting] = useState(false);
	const [heroCompleting, setHeroCompleting] = useState(false);
	useEffect(() => {
		const prevHealth = prevHealthRef.current;
		prevHealthRef.current = health;

		if (health === "healthy" && prevHealth !== "healthy") {
			setHeroCompleting(true);
			const t1 = setTimeout(() => {
				setHeroCompleting(false);
				setHeroExiting(true);
			}, 750);
			const t2 = setTimeout(() => {
				setHeroVisible(false);
				setHeroExiting(false);
			}, 750 + 450);
			return () => {
				clearTimeout(t1);
				clearTimeout(t2);
			};
		}
		if (health !== "healthy") {
			setHeroVisible(true);
			setHeroExiting(false);
			setHeroCompleting(false);
		}
	}, [health]);

	const pulseDelay = usePulseDelay(health === "building");

	if (!heroVisible || hasUndeployedChanges) return null;
	return (
		<output
			aria-label="Service status"
			{...stylex.props([
				styles.statusBadge,
				heroExiting ? styles.statusExiting : styles.statusEntering,
				health === "building"
					? styles.buildingBadge
					: health === "failed"
						? styles.failedBadge
						: styles.neutralBadge,
			])}
		>
			{heroCompleting && <span {...stylex.props(styles.completionGlimmer)} />}
			<span
				{...stylex.props([
					styles.statusLabel,
					health === "building"
						? styles.buildingLabel
						: health === "failed"
							? styles.failedLabel
							: health === "healthy"
								? styles.healthyLabel
								: styles.neutralLabel,
				])}
			>
				{serviceStatusLabel(currentService)}
			</span>
			{health === "building" && stages.length > 0 && (
				<DeploymentProgress
					stages={stages}
					label="Deploy progress"
					completing={heroCompleting}
					delay={pulseDelay}
				/>
			)}
		</output>
	);
}
