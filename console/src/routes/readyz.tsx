import { createFileRoute } from "@tanstack/react-router";

import { checkDashboardReadiness } from "#/lib/dashboard/server";

export const Route = createFileRoute("/readyz")({
	server: {
		handlers: {
			GET: async () => {
				const report = await checkDashboardReadiness();
				return Response.json(report, {
					status: report.status === "ready" ? 200 : 503,
					headers: {
						"cache-control": "no-store",
					},
				});
			},
		},
	},
	component: ReadyPage,
});

function ReadyPage() {
	return null;
}
