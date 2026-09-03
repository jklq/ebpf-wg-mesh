import {
	createRootRoute,
	type ErrorComponentProps,
	HeadContent,
	Scripts,
} from "@tanstack/react-router";
import type { ReactNode } from "react";

import { cn } from "#/lib/cn";
import { btnPrimary, modalCard } from "#/lib/ui-classes";

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
		<main
			className="grid min-h-screen place-items-center bg-canvas p-8"
			role="alert"
		>
			<div className={cn(modalCard, "max-w-xl p-8")}>
				<h1 className="mt-0 mb-2 font-display text-2xl font-medium text-ink">
					The console hit a temporary error
				</h1>
				<p className="mb-4 text-muted">
					Your service is still running. Retry the failed view without leaving
					the console.
				</p>
				<pre className="mb-4 max-h-48 overflow-auto whitespace-pre-wrap font-mono text-sm text-ink">
					{error instanceof Error ? error.message : String(error)}
				</pre>
				<button type="button" className={btnPrimary} onClick={reset}>
					Retry
				</button>
			</div>
		</main>
	);
}

function RootDocument({ children }: { children: ReactNode }) {
	return (
		<html lang="en" className="h-full min-h-full" suppressHydrationWarning>
			<head>
				<HeadContent />
			</head>
			<body className="h-full min-h-full bg-canvas font-sans text-[14px] text-ink antialiased">
				{children}
				<Scripts />
			</body>
		</html>
	);
}
