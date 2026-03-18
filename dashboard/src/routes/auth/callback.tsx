import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";

export interface AuthCallbackService {
	completeDevLogin(input: {
		subject: string;
		email: string;
		redirectTo?: string;
	}): Promise<string>;
}

export async function completeLoginRoute(
	service: AuthCallbackService,
	data: {
		subject: string;
		email: string;
		redirectTo?: string;
	},
): Promise<string> {
	return service.completeDevLogin(data);
}

const completeLogin = createServerFn({ method: "GET" })
	.inputValidator((input: unknown) => {
		const data = (input ?? {}) as Record<string, unknown>;
		return {
			subject: typeof data.subject === "string" ? data.subject : "",
			email: typeof data.email === "string" ? data.email : "",
			redirectTo: typeof data.redirectTo === "string" ? data.redirectTo : "/",
		};
	})
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard.server");
		return completeLoginRoute(service, data);
	});

export const Route = createFileRoute("/auth/callback")({
	validateSearch: (search: Record<string, unknown>) => ({
		subject: typeof search.subject === "string" ? search.subject : "",
		email: typeof search.email === "string" ? search.email : "",
		redirectTo: typeof search.redirect === "string" ? search.redirect : "/",
	}),
	loader: async ({ location }) => {
		const query = new URLSearchParams(location.search);
		const destination = await completeLogin({
			data: {
				subject: query.get("subject") ?? "",
				email: query.get("email") ?? "",
				redirectTo: query.get("redirect") ?? "/",
			},
		});
		throw redirect({ href: destination });
	},
	component: CallbackPage,
});

function CallbackPage() {
	return (
		<main className="flex min-h-screen items-center justify-center px-4">
			<div className="rounded-2xl border border-[var(--line)] bg-white px-5 py-4 text-sm text-[var(--muted)]">
				Completing sign-in...
			</div>
		</main>
	);
}
