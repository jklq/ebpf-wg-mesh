import type { ReactNode } from "react";
import { useEffect, useState, useTransition } from "react";
import { createFileRoute, redirect, useRouter } from "@tanstack/react-router";
import { createServerFn } from "@tanstack/react-start";
import { Schema } from "effect";

import type {
	DashboardHomeState,
	DashboardOnboardingDraft,
	DashboardServiceStatus,
} from "#/lib/dashboard.server";

export interface HomeRouteService {
	loadDashboardHome(): Promise<DashboardHomeState | null>;
	inspectRepositoryFromSession(input: {
		repositorySelector: string;
	}): Promise<DashboardOnboardingDraft>;
	confirmRepositoryFromSession(input: {
		repositorySelector: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		containerPort?: string;
	}): Promise<DashboardOnboardingDraft>;
	saveHostnameFromSession(hostname: string): Promise<DashboardOnboardingDraft>;
	publishDomainFromSession(): Promise<void>;
}

const RepositorySelectionSchema = Schema.Struct({
	repositorySelector: Schema.String,
});
const ConfirmRepositorySchema = Schema.Struct({
	repositorySelector: Schema.String,
	trackedRef: Schema.optionalKey(Schema.String),
	dockerfilePath: Schema.optionalKey(Schema.String),
	contextDir: Schema.optionalKey(Schema.String),
	containerPort: Schema.optionalKey(Schema.String),
});
const HostnameSchema = Schema.Struct({
	hostname: Schema.String,
});

const decodeRepositorySelection = Schema.decodeUnknownSync(
	RepositorySelectionSchema,
);
const decodeConfirmRepository = Schema.decodeUnknownSync(
	ConfirmRepositorySchema,
);
const decodeHostname = Schema.decodeUnknownSync(HostnameSchema);

export async function loadHomeRouteState(
	service: Pick<HomeRouteService, "loadDashboardHome">,
): Promise<DashboardHomeState> {
	const state = await service.loadDashboardHome();
	if (!state) {
		throw redirect({
			to: "/login",
			search: { redirect: undefined, error: undefined },
		});
	}
	return state;
}

const loadHome = createServerFn({ method: "GET" }).handler(async () => {
	const service = await import("#/lib/dashboard.server");
	return loadHomeRouteState(service);
});

const inspectRepository = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => decodeRepositorySelection(input ?? {}))
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard.server");
		return service.inspectRepositoryFromSession({
			repositorySelector: data.repositorySelector,
		});
	});

const confirmRepository = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => decodeConfirmRepository(input ?? {}))
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard.server");
		return service.confirmRepositoryFromSession(data);
	});

const saveHostname = createServerFn({ method: "POST" })
	.inputValidator((input: unknown) => decodeHostname(input ?? {}))
	.handler(async ({ data }) => {
		const service = await import("#/lib/dashboard.server");
		return service.saveHostnameFromSession(data.hostname);
	});

const publishDomain = createServerFn({ method: "POST" }).handler(async () => {
	const service = await import("#/lib/dashboard.server");
	await service.publishDomainFromSession();
	return null;
});

export const Route = createFileRoute("/")({
	loader: async () => loadHome(),
	component: HomePage,
});

function HomePage() {
	const state = Route.useLoaderData();
	const router = useRouter();
	const [repositorySelector, setRepositorySelector] = useState(
		state.onboarding.repositorySelector,
	);
	const [trackedRef, setTrackedRef] = useState(state.onboarding.trackedRef);
	const [dockerfilePath, setDockerfilePath] = useState(
		state.onboarding.dockerfilePath,
	);
	const [contextDir, setContextDir] = useState(state.onboarding.contextDir);
	const [containerPort, setContainerPort] = useState(
		state.onboarding.containerPort,
	);
	const [hostname, setHostname] = useState(state.onboarding.hostname);
	const [repoError, setRepoError] = useState<string>();
	const [domainError, setDomainError] = useState<string>();
	const [isPending, startTransition] = useTransition();

	useEffect(() => {
		setRepositorySelector(state.onboarding.repositorySelector);
		setTrackedRef(state.onboarding.trackedRef);
		setDockerfilePath(state.onboarding.dockerfilePath);
		setContextDir(state.onboarding.contextDir);
		setContainerPort(state.onboarding.containerPort);
		setHostname(state.onboarding.hostname);
	}, [
		state.onboarding.containerPort,
		state.onboarding.contextDir,
		state.onboarding.dockerfilePath,
		state.onboarding.hostname,
		state.onboarding.repositorySelector,
		state.onboarding.trackedRef,
	]);

	useEffect(() => {
		if (!shouldAutoRefresh(state)) {
			return undefined;
		}
		const interval = window.setInterval(() => {
			void router.invalidate();
		}, 5000);
		return () => window.clearInterval(interval);
	}, [router, state]);

	const handleRefresh = () => {
		startTransition(() => {
			void router.invalidate();
		});
	};

	const handleInspectRepository = () => {
		setRepoError(undefined);
		startTransition(() => {
			void inspectRepository({
				data: { repositorySelector },
			})
				.then(() => router.invalidate())
				.catch((error) => setRepoError(formatClientError(error)));
		});
	};

	const handleConfirmRepository = () => {
		setRepoError(undefined);
		startTransition(() => {
			void confirmRepository({
				data: {
					repositorySelector,
					trackedRef,
					dockerfilePath,
					contextDir,
					containerPort,
				},
			})
				.then(() => router.invalidate())
				.catch((error) => setRepoError(formatClientError(error)));
		});
	};

	const handleCheckDNS = () => {
		setDomainError(undefined);
		startTransition(() => {
			void saveHostname({
				data: { hostname },
			})
				.then(() => router.invalidate())
				.catch((error) => setDomainError(formatClientError(error)));
		});
	};

	const handlePublishDomain = () => {
		setDomainError(undefined);
		startTransition(() => {
			void publishDomain()
				.then(() => router.invalidate())
				.catch((error) => setDomainError(formatClientError(error)));
		});
	};

	return (
		<HomePageView
			state={state}
			repositorySelector={repositorySelector}
			trackedRef={trackedRef}
			dockerfilePath={dockerfilePath}
			contextDir={contextDir}
			containerPort={containerPort}
			hostname={hostname}
			repoError={repoError}
			domainError={domainError}
			isPending={isPending}
			onRepositorySelectorChange={setRepositorySelector}
			onTrackedRefChange={setTrackedRef}
			onDockerfilePathChange={setDockerfilePath}
			onContextDirChange={setContextDir}
			onContainerPortChange={setContainerPort}
			onHostnameChange={setHostname}
			onInspectRepository={handleInspectRepository}
			onConfirmRepository={handleConfirmRepository}
			onCheckDNS={handleCheckDNS}
			onPublishDomain={handlePublishDomain}
			onRefresh={handleRefresh}
		/>
	);
}

export function HomePageView({
	state,
	repositorySelector,
	trackedRef,
	dockerfilePath,
	contextDir,
	containerPort,
	hostname,
	repoError,
	domainError,
	isPending,
	onRepositorySelectorChange,
	onTrackedRefChange,
	onDockerfilePathChange,
	onContextDirChange,
	onContainerPortChange,
	onHostnameChange,
	onInspectRepository,
	onConfirmRepository,
	onCheckDNS,
	onPublishDomain,
	onRefresh,
}: {
	state: DashboardHomeState;
	repositorySelector: string;
	trackedRef: string;
	dockerfilePath: string;
	contextDir: string;
	containerPort: string;
	hostname: string;
	repoError?: string;
	domainError?: string;
	isPending: boolean;
	onRepositorySelectorChange: (value: string) => void;
	onTrackedRefChange: (value: string) => void;
	onDockerfilePathChange: (value: string) => void;
	onContextDirChange: (value: string) => void;
	onContainerPortChange: (value: string) => void;
	onHostnameChange: (value: string) => void;
	onInspectRepository: () => void;
	onConfirmRepository: () => void;
	onCheckDNS: () => void;
	onPublishDomain: () => void;
	onRefresh: () => void;
}) {
	const repositoryStep = repositoryStepState(state, repoError);
	const buildStep = buildStepState(state);
	const domainStep = domainStepState(state, domainError);
	const customBinding = state.domainBindings.find(
		(binding) => binding.hostname === state.onboarding.hostname,
	);
	const publishedURL = customBinding
		? publishedServiceURL(
				state.publicBaseURL,
				state.localIngressBaseURL,
				state.localDomainSuffix,
				customBinding.hostname,
			)
		: undefined;

	return (
		<main className="mx-auto flex min-h-screen w-full max-w-3xl flex-col gap-4 px-4 py-6 sm:px-6">
			<header className="flex items-center justify-between border border-[var(--line)] bg-white px-4 py-3">
				<div>
					<h1 className="text-lg font-semibold">Console onboarding</h1>
					<p className="text-sm text-[var(--muted)]">
						GitHub sign-in to live custom domain, with one primary action per
						step.
					</p>
				</div>
				<a
					href="/logout"
					className="border border-[var(--line)] px-3 py-2 text-sm"
				>
					Sign out
				</a>
			</header>

			{state.controlPlaneReachable ? null : (
				<section className="border border-[var(--danger)] bg-white px-4 py-3 text-sm text-[var(--danger)]">
					{state.controlPlaneError ?? "The control plane is unavailable."}
				</section>
			)}

			<OnboardingCard
				title="1. Account"
				status={
					state.githubAccount
						? `Signed in as ${state.githubAccount.login}`
						: `Signed in as ${state.user.email}`
				}
				description={
					state.githubAccount
						? "GitHub is connected. Repository picker and private repo access are available when your token allows it."
						: "GitHub sign-in is recommended for the repo picker. Manual owner/repo entry still works for public repos."
				}
				primaryAction={
					<a
						href="/login"
						className="inline-flex border border-[var(--line)] px-3 py-2 text-sm"
					>
						Manage sign-in
					</a>
				}
			/>

			<OnboardingCard
				title="2. Repository"
				status={repositoryStep.status}
				description="Choose one of your GitHub repos or paste owner/repo for a public repository, then confirm the detected build settings."
				primaryAction={
					repositoryStep.primaryAction === "confirm" ? (
						<button
							type="button"
							className="border border-[var(--ink)] bg-[var(--ink)] px-3 py-2 text-sm text-white disabled:opacity-50"
							onClick={onConfirmRepository}
							disabled={isPending || !repositoryStep.canConfirm}
						>
							{state.service ? "Apply service config" : "Create service"}
						</button>
					) : repositoryStep.primaryAction === "install" &&
						state.githubInstallURL ? (
						<a
							href={state.githubInstallURL}
							target="_blank"
							rel="noreferrer"
							className="inline-flex border border-[var(--ink)] bg-[var(--ink)] px-3 py-2 text-sm text-white"
						>
							Install GitHub App
						</a>
					) : (
						<button
							type="button"
							className="border border-[var(--ink)] bg-[var(--ink)] px-3 py-2 text-sm text-white disabled:opacity-50"
							onClick={onInspectRepository}
							disabled={isPending || repositorySelector.trim() === ""}
						>
							Check repository
						</button>
					)
				}
				secondaryAction={
					<button
						type="button"
						className="border border-[var(--line)] px-3 py-2 text-sm disabled:opacity-50"
						onClick={onRefresh}
						disabled={isPending}
					>
						Refresh
					</button>
				}
			>
				<div className="grid gap-3">
					<label className="grid gap-1 text-sm">
						<span>Your repos</span>
						<select
							value={
								state.repositories.some(
									(repository) => repository.fullName === repositorySelector,
								)
									? repositorySelector
									: ""
							}
							onChange={(event) =>
								onRepositorySelectorChange(event.target.value)
							}
							className="border border-[var(--line)] bg-white px-3 py-2"
						>
							<option value="">Choose a repo</option>
							{state.repositories.map((repository) => (
								<option key={repository.fullName} value={repository.fullName}>
									{repository.fullName}
								</option>
							))}
						</select>
					</label>

					<label className="grid gap-1 text-sm">
						<span>Or paste owner/repo</span>
						<input
							value={repositorySelector}
							onChange={(event) =>
								onRepositorySelectorChange(event.target.value)
							}
							placeholder="owner/repo"
							className="border border-[var(--line)] bg-white px-3 py-2"
						/>
					</label>

					{state.repositoryInspection ? (
						<details className="border border-[var(--line)] px-3 py-2">
							<summary className="cursor-pointer text-sm font-medium">
								Advanced
							</summary>
							<div className="mt-3 grid gap-3">
								<label className="grid gap-1 text-sm">
									<span>Branch</span>
									<input
										value={trackedRef}
										onChange={(event) => onTrackedRefChange(event.target.value)}
										className="border border-[var(--line)] bg-white px-3 py-2"
									/>
								</label>
								<label className="grid gap-1 text-sm">
									<span>Dockerfile path</span>
									<input
										value={dockerfilePath}
										onChange={(event) =>
											onDockerfilePathChange(event.target.value)
										}
										className="border border-[var(--line)] bg-white px-3 py-2"
									/>
								</label>
								<label className="grid gap-1 text-sm">
									<span>Context dir</span>
									<input
										value={contextDir}
										onChange={(event) => onContextDirChange(event.target.value)}
										className="border border-[var(--line)] bg-white px-3 py-2"
									/>
								</label>
								<label className="grid gap-1 text-sm">
									<span>Container port</span>
									<input
										value={containerPort}
										onChange={(event) =>
											onContainerPortChange(event.target.value)
										}
										placeholder="8080"
										inputMode="numeric"
										className="border border-[var(--line)] bg-white px-3 py-2"
									/>
								</label>
								{state.repositoryInspection.dockerfileCandidates.length > 0 ? (
									<p className="text-xs text-[var(--muted)]">
										Detected Dockerfiles:{" "}
										{state.repositoryInspection.dockerfileCandidates.join(", ")}
									</p>
								) : (
									<p className="text-xs text-[var(--danger)]">
										No Dockerfile was detected in the default branch.
									</p>
								)}
							</div>
						</details>
					) : null}

					{repoError ? (
						<p className="text-sm text-[var(--danger)]">{repoError}</p>
					) : null}
				</div>
			</OnboardingCard>

			<OnboardingCard
				title="3. Build"
				status={buildStep.status}
				description="The latest build must succeed and the allocation must turn healthy before the flow unlocks the domain step."
				primaryAction={
					<button
						type="button"
						className="border border-[var(--ink)] bg-[var(--ink)] px-3 py-2 text-sm text-white disabled:opacity-50"
						onClick={onRefresh}
						disabled={isPending}
					>
						Refresh build
					</button>
				}
				secondaryAction={
					buildStep.detail ? (
						<span className="text-xs text-[var(--muted)]">
							{buildStep.detail}
						</span>
					) : undefined
				}
			/>

			<OnboardingCard
				title="4. Domain"
				status={domainStep.status}
				description="Enter the hostname you want to publish. The console verifies DNS on the server before it creates the binding."
				primaryAction={
					domainStep.primaryAction === "publish" ? (
						<button
							type="button"
							className="border border-[var(--ink)] bg-[var(--ink)] px-3 py-2 text-sm text-white disabled:opacity-50"
							onClick={onPublishDomain}
							disabled={isPending}
						>
							Publish domain
						</button>
					) : (
						<button
							type="button"
							className="border border-[var(--ink)] bg-[var(--ink)] px-3 py-2 text-sm text-white disabled:opacity-50"
							onClick={onCheckDNS}
							disabled={isPending || hostname.trim() === "" || !buildStep.ready}
						>
							Check DNS
						</button>
					)
				}
				secondaryAction={
					<button
						type="button"
						className="border border-[var(--line)] px-3 py-2 text-sm disabled:opacity-50"
						onClick={onRefresh}
						disabled={isPending}
					>
						Refresh
					</button>
				}
			>
				<div className="grid gap-3">
					<label className="grid gap-1 text-sm">
						<span>Hostname</span>
						<input
							value={hostname}
							onChange={(event) => onHostnameChange(event.target.value)}
							placeholder="app.example.com"
							className="border border-[var(--line)] bg-white px-3 py-2"
						/>
					</label>

					{state.domainVerification ? (
						<div className="border border-[var(--line)] px-3 py-2 text-sm">
							<p>{state.domainVerification.instruction}</p>
							<p className="mt-2 text-[var(--muted)]">
								Observed state:{" "}
								{domainVerificationLabel(state.domainVerification.state)}
							</p>
						</div>
					) : null}

					{customBinding ? (
						<div className="border border-[var(--line)] px-3 py-2 text-sm">
							<p>Published URLs</p>
							{publishedURL ? (
								<a
									href={publishedURL}
									target="_blank"
									rel="noreferrer"
									className="mt-1 inline-flex text-[var(--accent-strong)]"
								>
									{publishedURL}
								</a>
							) : null}
						</div>
					) : null}

					{domainError ? (
						<p className="text-sm text-[var(--danger)]">{domainError}</p>
					) : null}
				</div>
			</OnboardingCard>
		</main>
	);
}

function OnboardingCard({
	title,
	status,
	description,
	primaryAction,
	secondaryAction,
	children,
}: {
	title: string;
	status: string;
	description: string;
	primaryAction: ReactNode;
	secondaryAction?: ReactNode;
	children?: ReactNode;
}) {
	return (
		<section className="border border-[var(--line)] bg-white px-4 py-4">
			<div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
				<div className="space-y-1">
					<h2 className="text-base font-semibold">{title}</h2>
					<p className="text-sm text-[var(--muted)]">{description}</p>
					<p className="text-sm">{status}</p>
				</div>
				<div className="flex gap-2">
					{primaryAction}
					{secondaryAction}
				</div>
			</div>
			{children ? <div className="mt-4">{children}</div> : null}
		</section>
	);
}

function repositoryStepState(
	state: DashboardHomeState,
	error?: string,
): {
	status: string;
	primaryAction: "inspect" | "confirm" | "install";
	canConfirm: boolean;
} {
	if (error) {
		return { status: error, primaryAction: "inspect", canConfirm: false };
	}
	if (!state.onboarding.repositorySelector) {
		return {
			status: "Choose a repository to begin.",
			primaryAction: "inspect",
			canConfirm: false,
		};
	}
	const inspection = state.repositoryInspection;
	if (!inspection) {
		return {
			status: "Check repository access and build hints.",
			primaryAction: "inspect",
			canConfirm: false,
		};
	}
	if (inspection.accessState === "installation_required") {
		if (!state.githubInstallURL) {
			return {
				status:
					"GitHub sign-in succeeded, but this repository still needs a GitHub App installation or repository grant. Configure DASHBOARD_GITHUB_INSTALL_URL to show the install link here.",
				primaryAction: "inspect",
				canConfirm: false,
			};
		}
		return {
			status:
				"GitHub sign-in succeeded, but the GitHub App is not installed for this repository yet. Open the install flow, grant the repository, then return here. The page will re-check automatically.",
			primaryAction: "install",
			canConfirm: false,
		};
	}
	if (inspection.accessState !== "available") {
		return {
			status: "Repository access is still blocked.",
			primaryAction: "inspect",
			canConfirm: false,
		};
	}
	if (inspection.dockerfileCandidates.length === 0) {
		return {
			status: "No Dockerfile was detected on the default branch.",
			primaryAction: "inspect",
			canConfirm: false,
		};
	}
	if (state.service) {
		return {
			status: `Connected to ${state.service.name}. Confirm to update build and runtime settings.`,
			primaryAction: "confirm",
			canConfirm: true,
		};
	}
	return {
		status: "Repository access is available. Confirm to create the service.",
		primaryAction: "confirm",
		canConfirm: true,
	};
}

function buildStepState(state: DashboardHomeState): {
	status: string;
	detail?: string;
	ready: boolean;
} {
	if (!state.serviceStatus) {
		return {
			status: "Create a service to start the build.",
			ready: false,
		};
	}
	const latestBuild = state.serviceStatus.service.latestBuild;
	if (!latestBuild) {
		return {
			status: "Waiting for the first build to queue.",
			detail: state.serviceStatus.allocation?.phase,
			ready: false,
		};
	}
	switch (latestBuild.state) {
		case "queued":
			return {
				status: "Build queued.",
				detail: state.serviceStatus.allocation?.phase,
				ready: false,
			};
		case "running":
			return {
				status: "Build running.",
				detail: state.serviceStatus.allocation?.phase,
				ready: false,
			};
		case "failed":
			return {
				status: `Build failed: ${latestBuild.failureReason || "unknown reason"}`,
				ready: false,
			};
		case "succeeded":
			if (buildReady(state.serviceStatus)) {
				return {
					status: "Healthy and ready for domain.",
					detail: state.serviceStatus.allocation?.endpointAddr,
					ready: true,
				};
			}
			return {
				status: "Build succeeded, but the deployment is still unhealthy.",
				detail:
					state.serviceStatus.allocation?.message ||
					state.serviceStatus.allocation?.phase,
				ready: false,
			};
		default:
			return {
				status: "Waiting for deployment state.",
				detail: state.serviceStatus.allocation?.phase,
				ready: false,
			};
	}
}

function domainStepState(
	state: DashboardHomeState,
	error?: string,
): {
	status: string;
	primaryAction: "check" | "publish";
} {
	if (error) {
		return { status: error, primaryAction: "check" };
	}
	if (!buildReady(state.serviceStatus)) {
		return {
			status: "Wait for a healthy deployment before publishing a domain.",
			primaryAction: "check",
		};
	}
	const customBinding = state.domainBindings.find(
		(binding) => binding.hostname === state.onboarding.hostname,
	);
	if (customBinding) {
		return {
			status: `Published on ${customBinding.hostname}.`,
			primaryAction: "publish",
		};
	}
	if (!state.onboarding.hostname) {
		return {
			status: "Enter a hostname and check DNS.",
			primaryAction: "check",
		};
	}
	if (state.domainVerification?.state === "verified") {
		return {
			status: "DNS verified. Publish when ready.",
			primaryAction: "publish",
		};
	}
	if (state.domainVerification) {
		return {
			status: `DNS ${domainVerificationLabel(state.domainVerification.state)}.`,
			primaryAction: "check",
		};
	}
	return {
		status: "Check DNS before publishing.",
		primaryAction: "check",
	};
}

function buildReady(status: DashboardServiceStatus | undefined): boolean {
	return (
		status?.service.latestBuild?.state === "succeeded" &&
		status.allocation?.healthy === true
	);
}

function publishedServiceURL(
	publicBaseURL: string,
	localIngressBaseURL: string | undefined,
	localDomainSuffix: string | undefined,
	hostname: string,
): string {
	const baseURL =
		localIngressBaseURL &&
		localDomainSuffix &&
		hostname.toLowerCase().endsWith(`.${localDomainSuffix.toLowerCase()}`)
			? localIngressBaseURL
			: publicBaseURL;
	const url = new URL(baseURL);
	url.hostname = hostname;
	if (
		(url.protocol === "https:" && url.port === "443") ||
		(url.protocol === "http:" && url.port === "80")
	) {
		url.port = "";
	}
	return url.toString().replace(/\/$/, "");
}

function shouldAutoRefresh(state: DashboardHomeState): boolean {
	if (
		state.repositoryInspection?.accessState === "installation_required" &&
		state.onboarding.repositorySelector !== ""
	) {
		return true;
	}
	return Boolean(state.serviceStatus && !buildReady(state.serviceStatus));
}

function domainVerificationLabel(
	state:
		| "not_found"
		| "wrong_target"
		| "pending_propagation"
		| "verified"
		| undefined,
): string {
	switch (state) {
		case "not_found":
			return "not found";
		case "wrong_target":
			return "points to the wrong target";
		case "pending_propagation":
			return "is still propagating";
		case "verified":
			return "verified";
		default:
			return "is unknown";
	}
}

function formatClientError(error: unknown): string {
	if (error && typeof error === "object" && "message" in error) {
		return String(error.message);
	}
	return "Request failed.";
}
