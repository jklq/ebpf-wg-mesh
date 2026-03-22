import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { Schema } from "effect";

import type {
	DashboardHomeState,
	DevLoginIdentity,
} from "#/lib/dashboard.server";

export interface LoginRouteState {
	session: DashboardHomeState | null;
	devUsers: Array<DevLoginIdentity>;
	githubLoginEnabled: boolean;
	publicBaseURL: string;
}

export interface LoginRouteService {
	listDevLogins(): Array<DevLoginIdentity>;
	isGitHubLoginEnabled(): boolean;
	getPublicBaseURL(): string;
	loadDashboardHome(): Promise<DashboardHomeState | null>;
}

const LoginSearchSchema = Schema.Struct({
	redirect: Schema.optionalKey(Schema.String),
	error: Schema.optionalKey(Schema.String),
	detail: Schema.optionalKey(Schema.String),
});
const decodeLoginSearch = Schema.decodeUnknownSync(LoginSearchSchema);

export async function loadLoginRouteState(
	service: LoginRouteService,
): Promise<LoginRouteState> {
	return {
		session: await service.loadDashboardHome(),
		devUsers: service.listDevLogins(),
		githubLoginEnabled: service.isGitHubLoginEnabled(),
		publicBaseURL: service.getPublicBaseURL(),
	};
}

const loadLoginState = createServerFn({ method: "GET" }).handler(async () => {
	const service = await import("#/lib/dashboard.server");
	return loadLoginRouteState(service);
});

export const Route = createFileRoute("/login")({
	validateSearch: (search: Record<string, unknown>) => decodeLoginSearch(search),
	loader: async () => {
		const state = await loadLoginState();
		if (state.session) {
			throw redirect({ to: "/" });
		}
		return state;
	},
	component: LoginPage,
});

function LoginPage() {
	const state = Route.useLoaderData();
	const search = Route.useSearch();
	return (
		<LoginPageView
			state={state}
			redirectTo={search.redirect}
			error={search.error}
			errorDetail={search.detail}
		/>
	);
}

export function LoginPageView({
	state,
	redirectTo,
	error,
	errorDetail,
}: {
	state: LoginRouteState;
	redirectTo?: string;
	error?: string;
	errorDetail?: string;
}) {
	return (
		<main className="mx-auto flex min-h-screen w-full max-w-3xl items-center px-4 py-8 sm:px-6">
			<section className="w-full border border-[var(--line)] bg-white p-6 sm:p-8">
				<p className="text-[11px] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
					Sign in
				</p>
				<h1 className="mt-3 text-3xl font-semibold tracking-tight">
					Continue with GitHub
				</h1>
				<p className="mt-3 max-w-2xl text-sm text-[var(--muted)]">
					Sign in, choose a repository, wait for the build to turn healthy, and
					then publish a custom domain.
				</p>

				{error ? (
					<div className="mt-6 rounded-2xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
						{loginErrorMessage(error, errorDetail)}
					</div>
				) : null}

				<div className="mt-8 space-y-3">
					{state.githubLoginEnabled ? (
						<a
							href={buildGitHubAuthStartURL(
								state.publicBaseURL,
								redirectTo ?? "/",
							)}
							className="flex items-center justify-between border border-[var(--line)] px-4 py-4 transition hover:border-[var(--accent-strong)] hover:bg-[var(--surface-soft)]"
						>
							<div>
								<p className="font-medium">Continue with GitHub</p>
								<p className="mt-1 text-xs text-[var(--muted)]">
									Use your GitHub identity to unlock the repository picker and
									private repo access.
								</p>
							</div>
							<span className="text-sm font-medium text-[var(--accent-strong)]">
								Continue
							</span>
						</a>
					) : null}
					{state.devUsers.length === 0 && !state.githubLoginEnabled ? (
						<div className="rounded-2xl border border-dashed border-[var(--line)] bg-[var(--surface-soft)] px-4 py-6 text-sm text-[var(--muted)]">
							No dev login identities are configured. Set
							<code className="ml-1 rounded bg-white px-1.5 py-0.5 text-[var(--ink)]">
								DASHBOARD_DEV_USERS
							</code>
							in the managed service environment.
						</div>
					) : state.devUsers.length > 0 ? (
						<div className="border border-[var(--line)] px-4 py-4">
							<p className="text-sm font-medium">Development logins</p>
							<div className="mt-3 space-y-2">
								{state.devUsers.map((user) => (
									<a
										key={user.subject}
										href={`/auth/callback?subject=${encodeURIComponent(user.subject)}&email=${encodeURIComponent(user.email)}&redirect=${encodeURIComponent(redirectTo ?? "/")}`}
										className="flex items-center justify-between border border-[var(--line)] px-4 py-3 transition hover:border-[var(--accent-strong)] hover:bg-[var(--surface-soft)]"
									>
										<div>
											<p className="font-medium">{user.email}</p>
											<p className="mt-1 text-xs text-[var(--muted)]">
												{user.subject}
											</p>
										</div>
										<span className="text-sm font-medium text-[var(--accent-strong)]">
											Continue
										</span>
									</a>
								))}
							</div>
						</div>
					) : null}
				</div>
			</section>
		</main>
	);
}

function buildGitHubAuthStartURL(
	publicBaseURL: string,
	redirectTo: string,
): string {
	const url = new URL("/auth/start", publicBaseURL);
	url.searchParams.set("redirect", redirectTo);
	return url.toString();
}

function loginErrorMessage(code: string, detail?: string): string {
	switch (code) {
		case "missing_verified_email":
			return "GitHub did not provide a verified primary email for this account.";
		case "email_linked_to_other_github":
			return "That email is already linked to a different GitHub account.";
		case "ambiguous_existing_user":
			return "Multiple existing users match that verified email. Manual intervention is required.";
		case "invalid_signin_state":
			return "The sign-in request expired or was invalid. Start the GitHub flow again.";
		case "github_auth_unavailable":
			return "GitHub sign-in is not configured for this environment.";
		case "dev_auth_unavailable":
			return "Dev login is not available in this environment.";
		case "invalid_dev_login":
			return "That dev login identity is not allowed.";
		case "github_api_error":
			return detail || "GitHub sign-in failed.";
		default:
			return "Sign-in failed.";
	}
}
