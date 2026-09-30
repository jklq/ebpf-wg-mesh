import * as stylex from "@stylexjs/stylex";
import { ArrowLeft } from "lucide-react";
import type { ReactNode } from "react";
import { buttonStyles } from "#/components/ui/button";
import { colors, sizes, space } from "#/styles/tokens.stylex";

export function PageShell({
	title,
	icon,
	back = { href: "/", label: "Services" },
	actions,
	children,
	width = "standard",
}: {
	title: string;
	icon: ReactNode;
	back?: { href: string; label: string };
	actions?: ReactNode;
	children: ReactNode;
	width?: "standard" | "wide";
}) {
	return (
		<main {...stylex.props(styles.page)}>
			<header {...stylex.props(styles.toolbar)}>
				<a
					href={back.href}
					{...stylex.props(buttonStyles.base, buttonStyles.ghost)}
				>
					<ArrowLeft size={13} />
					{back.label}
				</a>
				<div {...stylex.props(styles.toolbarTitle)}>
					{icon}
					<strong>{title}</strong>
				</div>
				<div {...stylex.props(styles.spacer)} />
				{actions}
			</header>
			<section
				{...stylex.props(styles.content, width === "wide" && styles.wide)}
			>
				{children}
			</section>
		</main>
	);
}
const styles = stylex.create({
	page: { minHeight: "100dvh", backgroundColor: colors.canvas },
	toolbar: {
		position: "sticky",
		top: 0,
		zIndex: 30,
		display: "flex",
		minHeight: sizes.header,
		flexWrap: "wrap",
		alignItems: "center",
		gap: 10,
		borderBottomWidth: 1,
		borderBottomStyle: "solid",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		paddingInline: space.lg,
	},
	toolbarTitle: {
		marginLeft: space.sm,
		display: "flex",
		alignItems: "center",
		gap: 7,
	},
	spacer: { flex: 1 },
	content: {
		marginInline: "auto",
		display: "flex",
		width: "100%",
		maxWidth: 1000,
		flexDirection: "column",
		gap: 24,
		paddingInline: { default: 32, "@media (max-width: 639px)": space.lg },
		paddingBlock: 32,
	},
	wide: { maxWidth: 1200 },
});
