import { createFileRoute } from "@tanstack/react-router";

export interface LogoutRouteService {
	clearSession(): Promise<void>;
}

export async function logoutRouteResponse(
	service: LogoutRouteService,
): Promise<Response> {
	await service.clearSession();
	return new Response(null, {
		status: 302,
		headers: {
			Location: "/login",
		},
	});
}

export const Route = createFileRoute("/logout")({
	server: {
		handlers: {
			GET: async () => {
				const auth = await import("#/lib/dashboard/core/auth.server");
				const runtime = (
					await import("#/lib/dashboard/server")
				).getDashboardRuntime();
				return logoutRouteResponse({
					clearSession: () => auth.clearSession(runtime),
				});
			},
		},
	},
	component: LogoutPage,
});

function LogoutPage() {
	return (
		<main className="flex min-h-screen items-center justify-center bg-canvas px-4">
			<div className="rounded-sm border border-line bg-surface px-5 py-4 text-sm text-muted">
				Signing out...
			</div>
		</main>
	);
}
