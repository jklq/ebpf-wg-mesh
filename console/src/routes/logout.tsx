import * as stylex from "@stylexjs/stylex";
import { createFileRoute } from "@tanstack/react-router";
import { colors, shape, space } from "#/styles/tokens.stylex";

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
		<main {...stylex.props(styles.page)}>
			<div {...stylex.props(styles.statusCard)}>Signing out...</div>
		</main>
	);
}

const styles = stylex.create({
	page: {
		display: "flex",
		minHeight: "100vh",
		alignItems: "center",
		justifyContent: "center",
		backgroundColor: colors.canvas,
		paddingInline: space.lg,
	},
	statusCard: {
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		paddingInline: "1.25rem",
		paddingBlock: space.lg,
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		color: colors.muted,
	},
});
