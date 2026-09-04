import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { Zap } from "lucide-react";
import { cn } from "#/lib/cn";
import type {
	DashboardHomeState,
	DevLoginIdentity,
} from "#/lib/dashboard/core/types.server";
import { errorMsg } from "#/lib/ui-classes";

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
	const service = await import("#/lib/dashboard/server");
	return loadLoginRouteState(service);
});

export const Route = createFileRoute("/login")({
	validateSearch: (search: Record<string, unknown>) => parseLoginSearch(search),
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
		<main className="flex min-h-dvh items-center justify-center overflow-auto bg-canvas p-6">
			<div className="w-full max-w-[400px]">
				<div className="mb-8 flex items-center gap-2">
					<Zap size={16} className="text-accent" />
					<span className="font-condensed text-lg font-bold uppercase tracking-[0.14em] text-ink">
						mesh
					</span>
				</div>

				<div className="rounded-sm border border-line-bright bg-surface p-7">
					<h1 className="m-0 mb-1.5 font-condensed text-[22px] font-bold uppercase tracking-[0.04em] text-ink">
						Sign in
					</h1>
					<p className="mt-0 mb-6 text-[13px] leading-normal text-muted">
						Deploy services from GitHub repositories.
					</p>

					{error && (
						<div className={cn(errorMsg, "mb-5")}>
							{loginErrorMessage(error, errorDetail)}
						</div>
					)}

					<div className="flex flex-col gap-2.5">
						{state.githubLoginEnabled && (
							<a
								href={buildGitHubAuthStartURL(
									state.publicBaseURL,
									redirectTo ?? "/",
								)}
								className="flex cursor-pointer items-center justify-between rounded-none border border-line bg-surface-raised px-4 py-3.5 no-underline transition-[border-color,background-color] duration-100 hover:border-accent hover:bg-surface-hover"
							>
								<div>
									<p className="m-0 text-sm font-semibold tracking-[0.04em] text-ink">
										Continue with GitHub
									</p>
									<p className="mt-0.5 mb-0 text-[11px] text-muted">
										Access your repos and private images
									</p>
								</div>
								<span className="text-xs font-semibold text-accent">→</span>
							</a>
						)}

						{state.devUsers.length > 0 && (
							<div className="overflow-hidden rounded-none border border-line bg-surface-raised">
								<div className="border-b border-line px-4 py-2">
									<span className="font-condensed text-[10px] font-bold uppercase tracking-[0.1em] text-muted">
										Dev logins
									</span>
								</div>
								{state.devUsers.map((user) => (
									<a
										key={user.id}
										href={`/auth/callback?user_id=${encodeURIComponent(user.id)}&email=${encodeURIComponent(user.email)}&redirect=${encodeURIComponent(redirectTo ?? "/")}`}
										className="flex cursor-pointer items-center justify-between border-b border-line px-4 py-3 no-underline transition-colors duration-100 hover:bg-surface-hover"
									>
										<div>
											<p className="m-0 text-[13px] font-semibold text-ink">
												{user.email}
											</p>
											<p className="mt-px mb-0 font-mono text-[11px] text-muted">
												{user.id}
											</p>
										</div>
										<span className="text-xs font-semibold text-accent">→</span>
									</a>
								))}
							</div>
						)}

						{state.devUsers.length === 0 && !state.githubLoginEnabled && (
							<div className="rounded-none border border-line bg-surface-raised p-4 text-xs leading-relaxed text-muted">
								No sign-in methods are configured. Set{" "}
								<code className="rounded-none border border-line bg-canvas px-1.5 py-px font-mono text-[11px]">
									DASHBOARD_DEV_USERS
								</code>{" "}
								or GitHub OAuth in the environment.
							</div>
						)}
					</div>
				</div>
			</div>
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
			return "Multiple existing users match that verified email.";
		case "invalid_signin_state":
			return "The sign-in request expired or was invalid. Try again.";
		case "github_auth_unavailable":
			return "GitHub sign-in is not configured for this environment.";
		case "dev_auth_unavailable":
			return "Dev login is not available in this environment.";
		case "invalid_dev_login":
			return "That dev login identity is not allowed.";
		case "github_api_error":
			return detail ?? "GitHub sign-in failed.";
		default:
			return "Sign-in failed.";
	}
}

function parseLoginSearch(input: unknown): {
	redirect?: string;
	error?: string;
	detail?: string;
} {
	if (!input || typeof input !== "object" || Array.isArray(input)) {
		return {};
	}
	const data = input as Record<string, unknown>;
	return {
		redirect: typeof data.redirect === "string" ? data.redirect : undefined,
		error: typeof data.error === "string" ? data.error : undefined,
		detail: typeof data.detail === "string" ? data.detail : undefined,
	};
}
