import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { z } from "zod";

const beginLogin = createServerFn({ method: "GET" })
	.inputValidator((input: unknown) => {
		const data = parseAuthStartInput(input);
		return {
			redirect: data.redirect ?? "/",
		};
	})
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard/server");
		return service.beginGitHubLogin({
			redirectTo: data.redirect,
		});
	});

export const Route = createFileRoute("/auth/start")({
	validateSearch: (search: Record<string, unknown>) => {
		const data = parseAuthStartInput(search);
		return {
			redirect: data.redirect ?? "/",
		};
	},
	loader: async ({ location }) => {
		const query = new URLSearchParams(location.search);
		const href = await beginLogin({
			data: { redirect: query.get("redirect") ?? "/" },
		});
		throw redirect({ href });
	},
	component: AuthStartPage,
});

function AuthStartPage() {
	return null;
}

function parseAuthStartInput(input: unknown): { redirect?: string } {
	return z.object({ redirect: z.string().optional() }).parse(input);
}
