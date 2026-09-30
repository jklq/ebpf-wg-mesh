import * as stylex from "@stylexjs/stylex";
import { Loader2, RefreshCw, X } from "lucide-react";
import { Button } from "#/components/ui/button";
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
	content: {
		display: "flex",
		height: "100%",
		flexDirection: "column",
		gap: space.lg,
		overflowY: "auto",
		paddingInline: "18px",
		paddingTop: "18px",
		paddingBottom: "1.75rem",
	},
	loading: { display: "flex", alignItems: "center" },
});
export function ServicePanelFallback({
	service,
	onClose,
	onRefresh,
}: {
	service: DashboardServiceRecord;
	onClose?: () => void;
	onRefresh?: () => void;
}) {
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
			<div {...stylex.props(styles.body)}>
				<div {...stylex.props(styles.content)}>
					<div {...stylex.props(styles.loading)}>
						<Loader2 size={13} {...stylex.props(styles.spinner)} />
					</div>
				</div>
			</div>
		</>
	);
}
