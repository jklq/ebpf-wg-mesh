import * as stylex from "@stylexjs/stylex";
import {
	AlertCircle,
	HardDrive,
	History,
	LogOut,
	RefreshCw,
	Server,
	Zap,
} from "lucide-react";
import { Button, buttonStyles } from "#/components/ui/button";
import { DeployButton } from "#/features/dashboard/navigation/deploy-button";
import { EnvironmentSwitcher } from "#/features/dashboard/navigation/environment-switcher";
import { ProjectSwitcher } from "#/features/dashboard/navigation/project-switcher";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import { colors, fonts, sizes, space } from "#/styles/tokens.stylex";

export function Topbar({
	state,
	onNewService,
	onNewVolume,
	onPreloadNewService,
	onRefresh,
	onNewEnvironment,
	onEnvironmentsChanged,
	onNavigateEnvironment,
}: {
	state: DashboardHomeState;
	onNewService: () => void;
	onNewVolume?: () => void;
	onPreloadNewService?: () => void;
	onRefresh: () => void;
	onNewEnvironment: () => void;
	onEnvironmentsChanged: () => void;
	onNavigateEnvironment: (environmentId: string | null) => void;
}) {
	return (
		<div {...stylex.props(styles.toolbar)}>
			<div {...stylex.props(styles.brand)}>
				<Zap size={15} {...stylex.props(styles.brandIcon)} />
				<span {...stylex.props(styles.brandName)}>mesh</span>
			</div>

			{state.project && (
				<div {...stylex.props(styles.breadcrumb)}>
					<ProjectSwitcher state={state} />
					{state.environment && (
						<>
							<span {...stylex.props(styles.separator)}>/</span>
							<EnvironmentSwitcher
								state={state}
								onCreateEnvironment={onNewEnvironment}
								onChanged={onEnvironmentsChanged}
								onNavigateEnvironment={onNavigateEnvironment}
							/>
						</>
					)}
				</div>
			)}

			{state.services.length > 0 && (
				<span {...stylex.props(styles.serviceCount)}>
					{state.services.length}{" "}
					{state.services.length === 1 ? "service" : "services"}
				</span>
			)}

			{!state.controlPlaneReachable && (
				<span {...stylex.props(styles.offlineNotice)}>
					<AlertCircle size={11} /> Control plane offline
				</span>
			)}

			<div {...stylex.props(styles.spacer)} />

			{!state.project && (
				<a
					href="/deleted"
					{...stylex.props([
						buttonStyles.base,
						buttonStyles.ghost,
						styles.recoveryLink,
					])}
					title="Recently deleted"
				>
					<History size={13} /> Recently deleted
				</a>
			)}

			{state.canManageFleet && (
				<a
					href="/fleet"
					{...stylex.props([
						buttonStyles.base,
						buttonStyles.ghost,
						styles.recoveryLink,
					])}
					title="Agent fleet"
				>
					<Server size={13} /> Fleet
				</a>
			)}

			<Button type="button" variant="ghost" onClick={onRefresh}>
				<RefreshCw size={13} />
			</Button>

			{onNewVolume && state.controlPlaneReachable && (
				<Button
					type="button"
					variant="secondary"
					onClick={onNewVolume}
					title="Create a storage volume"
				>
					<HardDrive size={13} /> Volume
				</Button>
			)}

			<DeployButton
				state={state}
				onNewService={onNewService}
				onPreload={onPreloadNewService}
			/>

			<a
				href="/logout"
				{...stylex.props([
					buttonStyles.base,
					buttonStyles.ghost,
					styles.recoveryLink,
				])}
				title="Sign out"
			>
				<LogOut size={13} />
			</a>
		</div>
	);
}

const styles = stylex.create({
	toolbar: {
		position: "fixed",
		insetInline: "0rem",
		top: "0rem",
		zIndex: "40",
		display: "flex",
		height: sizes.header,
		alignItems: "center",
		gap: space.md,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		paddingInline: space.lg,
	},
	brand: {
		marginRight: space.sm,
		display: "flex",
		alignItems: "center",
		gap: "0.625rem",
	},
	brandIcon: { color: colors.accent },
	brandName: {
		fontFamily: fonts.condensed,
		fontSize: "1rem",
		lineHeight: "calc(1.5 / 1)",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.14em",
		color: colors.ink,
	},
	breadcrumb: {
		display: "flex",
		minWidth: "0rem",
		alignItems: "center",
		gap: space.sm,
	},
	separator: {
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		color: colors.dim,
	},
	serviceCount: {
		fontFamily: fonts.mono,
		fontSize: "11px",
		letterSpacing: "0.03em",
		color: colors.dim,
	},
	offlineNotice: {
		display: "flex",
		alignItems: "center",
		gap: space.xs,
		fontSize: "11px",
		color: colors.failed,
	},
	spacer: { flex: "1" },
	recoveryLink: {
		gap: space.xs,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
	},
});
