import * as stylex from "@stylexjs/stylex";
import { Loader2, X } from "lucide-react";
import { Button } from "#/components/ui/button";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { colors, fonts, sizes, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
const styles = stylex.create({
	header: {
		display: "flex",
		height: sizes.header,
		flexShrink: "0",
		alignItems: "center",
		gap: "0.375rem",
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.lg,
		paddingBlock: space.sm,
	},
	serviceName: {
		overflow: "hidden",
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
		letterSpacing: "-0.03em",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.ink,
	},
	headerSpacer: { flex: "1" },
	body: {
		display: "flex",
		minHeight: "0rem",
		flex: "1",
		alignItems: "center",
		justifyContent: "center",
		padding: space.xxl,
	},
	status: {
		display: "flex",
		maxWidth: "18rem",
		flexDirection: "column",
		alignItems: "center",
		textAlign: "center",
	},
	spinner: { animation: `${spin} 1s linear infinite`, color: colors.building },
	title: {
		marginTop: space.lg,
		fontFamily: fonts.display,
		fontSize: "1.125rem",
		lineHeight: "calc(1.75 / 1.125)",
		fontWeight: "500",
		color: colors.ink,
	},
	repository: {
		marginTop: space.xs,
		fontFamily: fonts.mono,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	description: {
		marginTop: space.md,
		fontSize: "0.75rem",
		lineHeight: "1.625",
		color: colors.dim,
	},
});
export function PendingServicePanel({
	service,
	onClose,
}: {
	service: DashboardServiceRecord;
	onClose: () => void;
}) {
	return (
		<>
			<div {...stylex.props(styles.header)}>
				<strong {...stylex.props(styles.serviceName)}>{service.name}</strong>
				<span {...stylex.props(styles.headerSpacer)} />
				<Button
					type="button"
					variant="panelIcon"
					onClick={onClose}
					title="Close service panel"
				>
					<X size={14} />
				</Button>
			</div>
			<div {...stylex.props(styles.body)}>
				<div {...stylex.props(styles.status)}>
					<Loader2 size={22} {...stylex.props(styles.spinner)} />
					<div {...stylex.props(styles.title)}>Creating service</div>
					<div {...stylex.props(styles.repository)}>
						{service.spec?.source?.sourceSpec?.repositorySelector}
					</div>
					<div {...stylex.props(styles.description)}>
						Preparing the service and its first undeployed configuration.
					</div>
				</div>
			</div>
		</>
	);
}
