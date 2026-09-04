import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { z } from "zod";

export interface AuthCallbackService {
	completeAuthCallback(input: {
		code?: string;
		state?: string;
		userId?: string;
		email?: string;
		redirectTo?: string;
	}): Promise<string>;
}

export async function completeLoginRoute(
	service: AuthCallbackService,
	data: {
		code?: string;
		state?: string;
		userId?: string;
		email?: string;
		redirectTo?: string;
	},
): Promise<string> {
	return service.completeAuthCallback(data);
}

const completeLogin = createServerFn({ method: "GET" })
	.inputValidator((input: unknown) => {
		const data = parseAuthCallbackInput(input);
		return {
			code: data.code ?? "",
			state: data.state ?? "",
			userId: data.userId ?? "",
			email: data.email ?? "",
			redirectTo: data.redirectTo ?? "/",
		};
	})
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard/server");
		try {
			return await completeLoginRoute(service, data);
		} catch (error) {
			if (
				error &&
				typeof error === "object" &&
				"code" in error &&
				typeof (error as { code?: unknown }).code === "string"
			) {
				return `/login?error=${encodeURIComponent((error as { code: string }).code)}`;
			}
			if (
				error &&
				typeof error === "object" &&
				"_tag" in error &&
				(error as { _tag?: unknown })._tag === "GitHubApiError"
			) {
				const detail = githubCallbackErrorDetail(error);
				return `/login?error=github_api_error&detail=${encodeURIComponent(detail)}`;
			}
			throw error;
		}
	});

export const Route = createFileRoute("/auth/callback")({
	validateSearch: (search: Record<string, unknown>) => {
		const data = parseAuthCallbackSearch(search);
		return {
			code: data.code ?? "",
			state: data.state ?? "",
			userId: data.userId ?? "",
			email: data.email ?? "",
			redirectTo: data.redirect ?? "/",
		};
	},
	loader: async ({ location }) => {
		const query = new URLSearchParams(location.search);
		const destination = await completeLogin({
			data: {
				code: query.get("code") ?? "",
				state: query.get("state") ?? "",
				userId: query.get("user_id") ?? "",
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
		<main className="flex min-h-screen items-center justify-center bg-canvas px-4">
			<div className="rounded-sm border border-line bg-surface px-5 py-4 text-sm text-muted">
				Completing sign-in...
			</div>
		</main>
	);
}

function githubCallbackErrorDetail(error: unknown): string {
	const status =
		error && typeof error === "object" && "status" in error
			? (error as { status?: unknown }).status
			: undefined;
	const operation =
		error && typeof error === "object" && "operation" in error
			? (error as { operation?: unknown }).operation
			: undefined;

	if (status === 403) {
		if (
			operation === "githubGET:/user/emails" ||
			operation === "fetchIdentity"
		) {
			return "GitHub denied access to the user's email addresses. Add Account permissions -> Email addresses -> Read-only, then save the app and re-authorize it.";
		}
		return "GitHub denied the app's user-auth request. Recheck the GitHub App permissions and re-authorize the app.";
	}
	if (status === 401) {
		return "GitHub rejected the user-auth token exchange. Recheck the Client ID, Client Secret, and callback URL.";
	}
	return "GitHub sign-in failed. Recheck the GitHub App callback URL, user permissions, and client credentials.";
}

function parseAuthCallbackInput(input: unknown): {
	code?: string;
	state?: string;
	userId?: string;
	email?: string;
	redirectTo?: string;
} {
	return z
		.object({
			code: z.string().optional(),
			state: z.string().optional(),
			userId: z.string().optional(),
			email: z.string().optional(),
			redirectTo: z.string().optional(),
		})
		.parse(input);
}

function parseAuthCallbackSearch(input: unknown): {
	code?: string;
	state?: string;
	userId?: string;
	email?: string;
	redirect?: string;
} {
	return z
		.object({
			code: z.string().optional(),
			state: z.string().optional(),
			userId: z.string().optional(),
			user_id: z.string().optional(),
			email: z.string().optional(),
			redirect: z.string().optional(),
		})
		.transform(({ user_id, ...data }) => ({
			...data,
			userId: data.userId ?? user_id,
		}))
		.parse(input);
}
