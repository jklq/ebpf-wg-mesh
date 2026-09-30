import { createFileRoute, redirect } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import {
	LoginPageView,
	type LoginRouteState,
} from "#/features/auth/login-page";
import type {
	DashboardHomeState,
	DevLoginIdentity,
} from "#/lib/dashboard/core/types.server";

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
	const home = await import("#/lib/dashboard/core/operations-home.server");
	return loadLoginRouteState({
		...service,
		loadDashboardHome: () =>
			home.loadDashboardHome(service.getDashboardRuntime()),
	});
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
