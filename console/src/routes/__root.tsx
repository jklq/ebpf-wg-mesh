import {
	createRootRoute,
	type ErrorComponentProps,
	HeadContent,
	Scripts,
} from "@tanstack/react-router";
import type { ReactNode } from "react";
import appCss from "../styles.css?url";

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
		<main className="route-error" role="alert">
			<div className="modal-card">
				<h1>The console hit a temporary error</h1>
				<p>
					Your service is still running. Retry the failed view without leaving
					the console.
				</p>
				<pre>{error instanceof Error ? error.message : String(error)}</pre>
				<button type="button" className="btn-primary" onClick={reset}>
					Retry
				</button>
			</div>
		</main>
	);
}

function RootDocument({ children }: { children: ReactNode }) {
	return (
		<html lang="en" suppressHydrationWarning>
			<head>
				<HeadContent />
			</head>
			<body>
				{children}
				<Scripts />
			</body>
		</html>
	);
}
