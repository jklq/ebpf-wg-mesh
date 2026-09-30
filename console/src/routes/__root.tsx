import * as stylex from "@stylexjs/stylex";
import {
	createRootRoute,
	type ErrorComponentProps,
	HeadContent,
	Scripts,
} from "@tanstack/react-router";
import type { ReactNode } from "react";
import { Button } from "#/components/ui/button";
import { dialogStyles } from "#/components/ui/dialog";
import { CreatedServiceCacheProvider } from "#/features/dashboard/state/created-service-cache";
import { DevStyles } from "#/styles/dev-styles";
import { colors, fonts, space } from "#/styles/tokens.stylex";
import appCss from "../styles/reset.css?url";

export const Route = createRootRoute({
	head: () => ({
		meta: [
			{ charSet: "utf-8" },
			{ name: "viewport", content: "width=device-width, initial-scale=1" },
			{ title: "Managed Dashboard" },
		],
		links: [{ rel: "stylesheet", href: appCss }],
	}),
	errorComponent: RootError,
	shellComponent: RootDocument,
});

function RootError({ error, reset }: ErrorComponentProps) {
	return (
		<main {...stylex.props(styles.errorPage)} role="alert">
			<div {...stylex.props([dialogStyles.card, styles.errorCard])}>
				<h1 {...stylex.props(styles.errorTitle)}>
					The console hit a temporary error
				</h1>
				<p {...stylex.props(styles.errorDescription)}>
					Your service is still running. Retry the failed view without leaving
					the console.
				</p>
				<pre {...stylex.props(styles.errorDetails)}>
					{error instanceof Error ? error.message : String(error)}
				</pre>
				<Button type="button" variant="primary" onClick={reset}>
					Retry
				</Button>
			</div>
		</main>
	);
}

function RootDocument({ children }: { children: ReactNode }) {
	return (
		<html lang="en" {...stylex.props(styles.document)} suppressHydrationWarning>
			<head>
				<HeadContent />
				<DevStyles />
			</head>
			<body {...stylex.props(styles.body)}>
				<CreatedServiceCacheProvider>{children}</CreatedServiceCacheProvider>
				<Scripts />
			</body>
		</html>
	);
}

const styles = stylex.create({
	errorPage: {
		display: "grid",
		minHeight: "100vh",
		placeItems: "center",
		backgroundColor: colors.canvas,
		padding: space.xxl,
	},
	errorCard: { maxWidth: "36rem", padding: space.xxl },
	errorTitle: {
		marginTop: "0rem",
		marginBottom: space.sm,
		fontFamily: fonts.display,
		fontSize: "1.5rem",
		lineHeight: "calc(2 / 1.5)",
		fontWeight: "500",
		color: colors.ink,
	},
	errorDescription: { marginBottom: space.lg, color: colors.muted },
	errorDetails: {
		marginBottom: space.lg,
		maxHeight: "12rem",
		overflow: "auto",
		whiteSpace: "pre-wrap",
		fontFamily: fonts.mono,
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		color: colors.ink,
	},
	document: { height: "100%", minHeight: "100%" },
	body: {
		height: "100%",
		minHeight: "100%",
		backgroundColor: colors.canvas,
		fontFamily: fonts.sans,
		fontSize: "14px",
		color: colors.ink,
		WebkitFontSmoothing: "antialiased",
		MozOsxFontSmoothing: "grayscale",
	},
});
