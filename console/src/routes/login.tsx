import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { Zap } from "lucide-react";

import type {
	DashboardHomeState,
	DevLoginIdentity,
} from "#/lib/dashboard/core/types.server";

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
		<main
			style={{
				minHeight: "100dvh",
				display: "flex",
				alignItems: "center",
				justifyContent: "center",
				padding: "24px",
				background: "var(--bg)",
				overflow: "auto",
			}}
		>
			<div style={{ width: "100%", maxWidth: 400 }}>
				{/* Brand */}
				<div
					style={{
						display: "flex",
						alignItems: "center",
						gap: 8,
						marginBottom: 32,
					}}
				>
					<Zap size={16} color="var(--accent)" />
					<span
						style={{
							fontSize: 18,
							fontWeight: 700,
							letterSpacing: "0.14em",
							textTransform: "uppercase",
							fontFamily: "'Barlow Condensed', sans-serif",
							color: "var(--text)",
						}}
					>
						mesh
					</span>
				</div>

				<div
					style={{
						background: "var(--surface)",
						border: "1px solid var(--border-bright)",
						borderRadius: 2,
						padding: "28px",
					}}
				>
					<h1
						style={{
							margin: "0 0 6px",
							fontSize: 22,
							fontWeight: 700,
							color: "var(--text)",
							letterSpacing: "0.04em",
							textTransform: "uppercase",
							fontFamily: "'Barlow Condensed', sans-serif",
						}}
					>
						Sign in
					</h1>
					<p
						style={{
							margin: "0 0 24px",
							fontSize: 13,
							color: "var(--text-muted)",
							lineHeight: 1.5,
						}}
					>
						Deploy services from GitHub repositories.
					</p>

					{error && (
						<div className="error-msg" style={{ marginBottom: 20 }}>
							{loginErrorMessage(error, errorDetail)}
						</div>
					)}

					<div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
						{state.githubLoginEnabled && (
							<a
								href={buildGitHubAuthStartURL(
									state.publicBaseURL,
									redirectTo ?? "/",
								)}
								style={{
									display: "flex",
									alignItems: "center",
									justifyContent: "space-between",
									padding: "14px 16px",
									background: "var(--surface-raised)",
									border: "1px solid var(--border)",
									borderRadius: 0,
									textDecoration: "none",
									transition: "border-color 0.1s ease, background 0.1s ease",
									cursor: "pointer",
								}}
								onMouseEnter={(e) => {
									(e.currentTarget as HTMLElement).style.borderColor =
										"var(--accent)";
									(e.currentTarget as HTMLElement).style.background =
										"var(--surface-hover)";
								}}
								onMouseLeave={(e) => {
									(e.currentTarget as HTMLElement).style.borderColor =
										"var(--border)";
									(e.currentTarget as HTMLElement).style.background =
										"var(--surface-raised)";
								}}
							>
								<div>
									<p
										style={{
											margin: 0,
											fontSize: 14,
											fontWeight: 600,
											letterSpacing: "0.04em",
											color: "var(--text)",
										}}
									>
										Continue with GitHub
									</p>
									<p
										style={{
											margin: "2px 0 0",
											fontSize: 11,
											color: "var(--text-muted)",
										}}
									>
										Access your repos and private images
									</p>
								</div>
								<span
									style={{
										fontSize: 12,
										fontWeight: 600,
										color: "var(--accent)",
									}}
								>
									→
								</span>
							</a>
						)}

						{state.devUsers.length > 0 && (
							<div
								style={{
									background: "var(--surface-raised)",
									border: "1px solid var(--border)",
									borderRadius: 0,
									overflow: "hidden",
								}}
							>
								<div
									style={{
										padding: "8px 16px",
										borderBottom: "1px solid var(--border)",
									}}
								>
									<span
										style={{
											fontSize: 10,
											fontWeight: 700,
											letterSpacing: "0.10em",
											textTransform: "uppercase",
											fontFamily: "'Barlow Condensed', sans-serif",
											color: "var(--text-muted)",
										}}
									>
										Dev logins
									</span>
								</div>
								{state.devUsers.map((user) => (
									<a
										key={user.subject}
										href={`/auth/callback?subject=${encodeURIComponent(user.subject)}&email=${encodeURIComponent(user.email)}&redirect=${encodeURIComponent(redirectTo ?? "/")}`}
										style={{
											display: "flex",
											alignItems: "center",
											justifyContent: "space-between",
											padding: "12px 16px",
											borderBottom: "1px solid var(--border)",
											textDecoration: "none",
											transition: "background 0.1s ease",
											cursor: "pointer",
										}}
										onMouseEnter={(e) => {
											(e.currentTarget as HTMLElement).style.background =
												"var(--surface-hover)";
										}}
										onMouseLeave={(e) => {
											(e.currentTarget as HTMLElement).style.background =
												"transparent";
										}}
									>
										<div>
											<p
												style={{
													margin: 0,
													fontSize: 13,
													fontWeight: 600,
													color: "var(--text)",
												}}
											>
												{user.email}
											</p>
											<p
												style={{
													margin: "1px 0 0",
													fontSize: 11,
													color: "var(--text-muted)",
													fontFamily: "var(--font-mono)",
												}}
											>
												{user.subject}
											</p>
										</div>
										<span
											style={{
												fontSize: 12,
												fontWeight: 600,
												color: "var(--accent)",
											}}
										>
											→
										</span>
									</a>
								))}
							</div>
						)}

						{state.devUsers.length === 0 && !state.githubLoginEnabled && (
							<div
								style={{
									padding: "16px",
									background: "var(--surface-raised)",
									border: "1px solid var(--border)",
									borderRadius: 0,
									fontSize: 12,
									color: "var(--text-muted)",
									lineHeight: 1.6,
								}}
							>
								No sign-in methods are configured. Set{" "}
								<code
									style={{
										fontFamily: "var(--font-mono)",
										background: "var(--bg)",
										padding: "1px 5px",
										borderRadius: 0,
										border: "1px solid var(--border)",
										fontSize: 11,
									}}
								>
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
