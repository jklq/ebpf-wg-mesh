// @vitest-environment jsdom

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

import { Topbar } from "./topbar";

afterEach(cleanup);

describe("Topbar", () => {
	it("only exposes fleet management to operators", () => {
		const { rerender } = renderTopbar(homeState({ canManageFleet: false }));

		expect(screen.queryByRole("link", { name: "Fleet" })).toBeNull();

		rerender(topbar(homeState({ canManageFleet: true })));
		expect(
			screen.getByRole<HTMLAnchorElement>("link", { name: "Fleet" }).href,
		).toBe("http://localhost:3000/fleet");
	});
});

function renderTopbar(state: DashboardHomeState) {
	return render(topbar(state));
}

function topbar(state: DashboardHomeState) {
	return (
		<Topbar
			state={state}
			onNewService={vi.fn()}
			onRefresh={vi.fn()}
			onNewEnvironment={vi.fn()}
			onEnvironmentsChanged={vi.fn()}
			onNavigateEnvironment={vi.fn()}
		/>
	);
}

function homeState(overrides: Partial<DashboardHomeState>): DashboardHomeState {
	return {
		user: { id: "user-1", email: "user@example.com" },
		onboarding: {
			projectId: "",
			environmentId: "",
			serviceId: "",
			repositorySelector: "",
			trackedRef: "main",
			builder: "BUILDER_KIND_DOCKERFILE",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		publicBaseURL: "https://dashboard.example.test",
		ingressTargetHost: "platform.example.test",
		environments: [],
		services: [],
		servicesRevision: 0,
		selectedServiceId: null,
		domainBindings: [],
		controlPlaneReachable: true,
		...overrides,
	};
}
