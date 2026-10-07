import { createFileRoute } from "@tanstack/react-router";
import { currentSession } from "#/lib/dashboard/core/auth.server";
import { getDashboardRuntime } from "#/lib/dashboard/server";

// Read-only authentication inspection remains available while mutations are
// paused, so production activation can verify the loaded session authority.
export const Route = createFileRoute("/sessionz")({
	server: {
		handlers: {
			GET: async () => {
				const session = await currentSession(getDashboardRuntime());
				return Response.json(
					session ? { userID: session.user.id } : { error: "unauthorized" },
					{
						status: session ? 200 : 401,
						headers: { "cache-control": "no-store" },
					},
				);
			},
		},
	},
	component: () => null,
});
