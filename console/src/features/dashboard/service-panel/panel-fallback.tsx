import * as stylex from "@stylexjs/stylex";
import { Loader2, RefreshCw, X } from "lucide-react";
import { useId } from "react";
import { Button } from "#/components/ui/button";
import { TabList } from "#/components/ui/tabs";
import { TabSkeleton } from "#/features/dashboard/service-panel/panel-skeleton";
import { servicePanelTabs } from "#/features/dashboard/service-panel/panel-tabs";
import type { DashboardTab } from "#/features/dashboard/shared/types";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { colors, fonts, sizes, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

const styles = stylex.create({
	headerSpacer: { flex: "1" },
	header: {
		display: "flex",
		height: sizes.header,
		flexShrink: "0",
		alignItems: "center",
		gap: "0.375rem",
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		backgroundImage:
			"linear-gradient(180deg,rgba(255,255,255,0.02),transparent)",
		backgroundColor: "rgba(24,23,21,0.92)",
		paddingInline: space.lg,
		paddingBlock: space.sm,
	},
	serviceName: {
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
		letterSpacing: "-0.03em",
		color: colors.ink,
	},
	spinner: { animation: `${spin} 1s linear infinite` },
	body: {
		position: "relative",
		minHeight: "0rem",
		flex: "1",
		overflow: "hidden",
	},
});
export function ServicePanelFallback({
	service,
	tab,
	onTabChange,
	onClose,
	onRefresh,
}: {
	service: DashboardServiceRecord;
	tab: DashboardTab;
	onTabChange: (tab: DashboardTab) => void;
	onClose?: () => void;
	onRefresh?: () => void;
}) {
	const tabsId = useId();
	return (
		<>
			<div {...stylex.props(styles.header)}>
				<strong {...stylex.props(styles.serviceName)}>{service.name}</strong>
				<span {...stylex.props(styles.headerSpacer)} />
				<Loader2 size={13} {...stylex.props(styles.spinner)} />
				<Button
					type="button"
					variant="panelIcon"
					onClick={onRefresh}
					disabled={!onRefresh}
					title="Refresh service"
				>
					<RefreshCw size={13} />
				</Button>
				<Button
					type="button"
					variant="panelIcon"
					onClick={onClose}
					disabled={!onClose}
					title="Close service panel"
				>
					<X size={14} />
				</Button>
			</div>
			<TabList
				items={servicePanelTabs}
				selected={tab}
				onSelect={onTabChange}
				label="Service"
				id={tabsId}
				panelId={`${tabsId}-panel`}
			/>
			<div
				role="tabpanel"
				id={`${tabsId}-panel`}
				aria-labelledby={`${tabsId}-${tab}`}
				{...stylex.props(styles.body)}
			>
				<TabSkeleton tab={tab} service={service} />
			</div>
		</>
	);
}
