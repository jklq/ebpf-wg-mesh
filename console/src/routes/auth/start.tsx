import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { Schema } from "effect";

const AuthStartInputSchema = Schema.Struct({
	redirect: Schema.optionalKey(Schema.String),
});
const decodeAuthStartInput = Schema.decodeUnknownSync(AuthStartInputSchema);

const beginLogin = createServerFn({ method: "GET" })
	.inputValidator((input: unknown) => {
		const data = decodeAuthStartInput(input ?? {});
		return {
			redirect: data.redirect ?? "/",
		};
	})
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard.server");
		return service.beginGitHubLogin({
			redirectTo: data.redirect,
		});
	});

export const Route = createFileRoute("/auth/start")({
	validateSearch: (search: Record<string, unknown>) => {
		const data = decodeAuthStartInput(search);
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
