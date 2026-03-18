import type { FormEvent } from "react";
import { useState, useTransition } from "react";
import { createFileRoute, redirect, useRouter } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";

import type {
	DashboardHomeState,
	DashboardProject,
} from "#/lib/dashboard.server";

export interface HomeRouteService {
	loadDashboardHome(): Promise<DashboardHomeState | null>;
	createProjectFromSession(name: string): Promise<DashboardProject>;
}

export async function loadHomeRouteState(
	service: Pick<HomeRouteService, "loadDashboardHome">,
): Promise<DashboardHomeState> {
	const state = await service.loadDashboardHome();
	if (!state) {
		throw redirect({ to: "/login", search: { redirect: undefined } });
	}
	return state;
}

const loadHome = createServerFn({ method: "GET" }).handler(async () => {
	const service = await import("#/lib/dashboard.server");
	return loadHomeRouteState(service);
});

const createProject = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => {
		const data = (input ?? {}) as { name?: unknown };
		return {
			name: typeof data.name === "string" ? data.name : "",
		};
	})
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard.server");
		return service.createProjectFromSession(data.name);
	});

export const Route = createFileRoute("/")({
	loader: async () => loadHome(),
	component: HomePage,
});

function HomePage() {
	const state = Route.useLoaderData();
	const router = useRouter();
	const [projectName, setProjectName] = useState("");
	const [submitError, setSubmitError] = useState<string>();
	const [isPending, startTransition] = useTransition();

	const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
		event.preventDefault();
		setSubmitError(undefined);
		const formData = new FormData(event.currentTarget);
		const submittedName = String(formData.get("projectName") ?? "");
		startTransition(async () => {
			try {
				await createProject({ data: { name: submittedName } });
				setProjectName("");
				await router.invalidate();
			} catch (error) {
				setSubmitError(formatClientError(error));
			}
		});
	};

	return (
		<HomePageView
			state={state}
			projectName={projectName}
			submitError={submitError}
			isPending={isPending}
			onProjectNameChange={setProjectName}
			onSubmit={handleSubmit}
		/>
	);
}

export function HomePageView({
	state,
	projectName,
	submitError,
	isPending,
	onProjectNameChange,
	onSubmit,
}: {
	state: DashboardHomeState;
	projectName: string;
	submitError?: string;
	isPending: boolean;
	onProjectNameChange: (value: string) => void;
	onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
	return (
		<main className="mx-auto flex min-h-screen w-full max-w-5xl flex-col gap-6 px-4 py-6 sm:px-6">
			<header className="rounded-3xl border border-[var(--line)] bg-white p-6 shadow-[0_10px_30px_rgba(15,23,42,0.06)]">
				<div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
					<div className="space-y-2">
						<p className="text-[11px] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
							Managed Dashboard
						</p>
						<h1 className="text-3xl font-semibold tracking-tight">
							Authenticated shell
						</h1>
						<p className="max-w-2xl text-sm text-[var(--muted)]">
							This page is intentionally thin. The dashboard owns the
							public-facing auth/session boundary and forwards orchestration
							actions to the control plane over internal mTLS gRPC.
						</p>
					</div>
					<a
						href="/logout"
						className="inline-flex h-10 items-center justify-center rounded-full border border-[var(--line)] px-4 text-sm font-medium text-[var(--ink)] transition hover:border-[var(--accent-strong)] hover:text-[var(--accent-strong)]"
					>
						Sign out
					</a>
				</div>
			</header>

			<section className="grid gap-6 lg:grid-cols-[1.1fr_0.9fr]">
				<article className="rounded-3xl border border-[var(--line)] bg-white p-6">
					<p className="text-[11px] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
						Me
					</p>
					<dl className="mt-4 grid gap-4 sm:grid-cols-2">
						<div>
							<dt className="text-xs uppercase tracking-[0.18em] text-[var(--muted)]">
								Subject
							</dt>
							<dd className="mt-1 font-medium">{state.user.subject}</dd>
						</div>
						<div>
							<dt className="text-xs uppercase tracking-[0.18em] text-[var(--muted)]">
								Email
							</dt>
							<dd className="mt-1 font-medium">{state.user.email}</dd>
						</div>
					</dl>
				</article>

				<article className="rounded-3xl border border-[var(--line)] bg-white p-6">
					<p className="text-[11px] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
						Control Plane
					</p>
					<div className="mt-4 rounded-2xl border border-dashed border-[var(--line)] bg-[var(--surface-soft)] p-4">
						<p className="text-sm font-medium">
							{state.controlPlaneReachable
								? "Internal PlatformService reachable"
								: "Internal PlatformService not reachable"}
						</p>
						<p className="mt-2 text-sm text-[var(--muted)]">
							{state.controlPlaneReachable
								? "Delegated principal projection and project listing succeeded for this session."
								: state.controlPlaneError}
						</p>
					</div>
				</article>
			</section>

			<section className="grid gap-6 lg:grid-cols-[1.1fr_0.9fr]">
				<article className="rounded-3xl border border-[var(--line)] bg-white p-6">
					<div className="flex items-center justify-between gap-4">
						<div>
							<p className="text-[11px] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
								Projects
							</p>
							<h2 className="mt-2 text-xl font-semibold">
								Visible user projects
							</h2>
						</div>
						<span className="rounded-full bg-[var(--surface-soft)] px-3 py-1 text-xs font-medium text-[var(--muted)]">
							{state.projects.length} listed
						</span>
					</div>
					<div className="mt-4 space-y-3">
						{state.projects.length === 0 ? (
							<div className="rounded-2xl border border-dashed border-[var(--line)] px-4 py-6 text-sm text-[var(--muted)]">
								No user-scoped projects are visible yet.
							</div>
						) : (
							state.projects.map((project) => (
								<div
									key={project.id}
									className="rounded-2xl border border-[var(--line)] px-4 py-4"
								>
									<div className="flex items-center justify-between gap-4">
										<div>
											<p className="font-medium">{project.name}</p>
											<p className="mt-1 text-xs text-[var(--muted)]">
												{project.id}
											</p>
										</div>
										<span className="rounded-full bg-[var(--surface-soft)] px-3 py-1 text-xs font-medium uppercase tracking-[0.14em] text-[var(--muted)]">
											{project.kind}
										</span>
									</div>
								</div>
							))
						)}
					</div>
				</article>

				<article className="rounded-3xl border border-[var(--line)] bg-white p-6">
					<p className="text-[11px] font-semibold uppercase tracking-[0.24em] text-[var(--accent-strong)]">
						Server Action
					</p>
					<h2 className="mt-2 text-xl font-semibold">Create a project</h2>
					<p className="mt-2 text-sm text-[var(--muted)]">
						This submits a user action through the dashboard backend and creates
						the project through the internal control-plane gRPC API.
					</p>
					<form className="mt-5 space-y-3" onSubmit={onSubmit}>
						<label className="block space-y-2">
							<span className="text-xs uppercase tracking-[0.18em] text-[var(--muted)]">
								Project name
							</span>
							<input
								name="projectName"
								value={projectName}
								onChange={(event) => onProjectNameChange(event.target.value)}
								placeholder="demo-app"
								className="w-full rounded-2xl border border-[var(--line)] bg-[var(--surface-soft)] px-4 py-3 outline-none transition focus:border-[var(--accent-strong)]"
							/>
						</label>
						{submitError ? (
							<p className="text-sm text-[var(--danger)]">{submitError}</p>
						) : null}
						<button
							type="submit"
							disabled={isPending}
							className="inline-flex h-11 items-center justify-center rounded-full bg-[var(--ink)] px-5 text-sm font-medium text-white transition hover:bg-[var(--accent-strong)] disabled:cursor-wait disabled:opacity-60"
						>
							{isPending ? "Creating..." : "Create project"}
						</button>
					</form>
				</article>
			</section>
		</main>
	);
}

function formatClientError(error: unknown): string {
	if (error && typeof error === "object" && "message" in error) {
		return String(error.message);
	}
	return "request failed";
}
