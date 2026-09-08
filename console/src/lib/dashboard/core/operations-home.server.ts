import { currentSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	loadOnboardingDraft,
	onboardingDraftEquals,
	platformCall,
	reconcileOnboardingDraft,
	safePlatformCall,
	saveOnboardingDraft,
	storeCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardHomeState,
	type DashboardServiceRecord,
	PlatformGatewayError,
} from "#/lib/dashboard/core/types.server";
import {
	applyServicePositions,
	publicGitHubAccount,
} from "./operations-helpers.server";

export async function loadDashboardHome(
	runtime: DashboardRuntime,
	selectedEnvironmentId?: string,
): Promise<DashboardHomeState | null> {
	const session = await currentSession(runtime);
	if (!session) {
		return null;
	}

	const { config } = runtime;
	const onboarding = await loadOnboardingDraft(runtime, session.user.id);
	const githubAccount = await storeCall(runtime, "getGitHubAccount", (store) =>
		store.getGitHubAccount(session.user.id),
	);

	const baseState = {
		user: session.user,
		canManageFleet: false,
		githubAccount: publicGitHubAccount(githubAccount),
		onboarding,
		repositories: [],
		githubLoginURL: runtime.github ? "/auth/start?redirect=%2F" : undefined,
		githubInstallURL: config.githubInstallURL,
		publicBaseURL: config.publicBaseURL,
		localIngressBaseURL: config.localIngressBaseURL,
		ingressTargetHost: config.ingressTargetHost,
		localDomainSuffix: config.localDomainSuffix,
		environments: [],
		services: [],
		servicesRevision: 0,
		selectedServiceId: null,
		domainBindings: [],
		controlPlaneReachable: true,
	} satisfies DashboardHomeState;

	try {
		const [projects, fleet] = await Promise.all([
			platformCall(runtime, "listProjects", (platform) =>
				platform.listProjects(session.user),
			),
			safePlatformCall(runtime, "listFleet", (platform) =>
				platform.listFleet(session.user),
			),
		]);
		const selectedEnvironment = selectedEnvironmentId
			? await safePlatformCall(runtime, "getEnvironment", (platform) =>
					platform.getEnvironment(session.user, selectedEnvironmentId),
				)
			: undefined;
		let project = selectedEnvironment
			? projects.find((entry) => entry.id === selectedEnvironment.projectId)
			: onboarding.projectId
				? projects.find((entry) => entry.id === onboarding.projectId)
				: undefined;
		if (!project && onboarding.repositorySelector) {
			project = projects.find(
				(entry) => entry.name === onboarding.repositorySelector,
			);
		}
		// Returning users (and product e2e fixtures) may already own projects
		// without an onboarding draft pointer.
		if (!project && projects.length > 0) {
			project = projects[0];
		}

		let reconciledDraft = onboarding;
		let environments: DashboardHomeState["environments"] = [];
		let environment: DashboardHomeState["environment"];
		let allServices: Array<DashboardServiceRecord> = [];
		let servicesRevision = 0;
		let selectedServiceId: string | null = null;

		if (project) {
			environments = await platformCall(
				runtime,
				"listEnvironments",
				(platform) => platform.listEnvironments(session.user, project.id),
			);
			environment =
				environments.find((entry) => entry.id === selectedEnvironment?.id) ??
				environments.find((entry) => entry.id === onboarding.environmentId) ??
				environments.find((entry) => entry.isProduction) ??
				environments[0];
		}

		if (environment) {
			const [servicesSnapshot, positions] = await Promise.all([
				safePlatformCall(runtime, "listServices", (platform) =>
					platform.listServices(session.user, environment.id),
				),
				storeCall(runtime, "listServicePositions", (store) =>
					store.listServicePositions(session.user.id, environment.id),
				),
			]);
			allServices = applyServicePositions(
				servicesSnapshot?.services ?? [],
				positions,
			);
			servicesRevision = servicesSnapshot?.index ?? 0;
		}

		const selectedService =
			project && onboarding.serviceId
				? allServices.find((s) => s.id === onboarding.serviceId)
				: undefined;
		const repositoryMatch =
			!selectedService && project && onboarding.repositorySelector
				? (() => {
						const matchingServices = allServices.filter(
							(entry) =>
								entry.spec?.source?.repositorySelector ===
								onboarding.repositorySelector,
						);
						return matchingServices.length === 1
							? matchingServices[0]
							: undefined;
					})()
				: undefined;
		const resolvedSelection = selectedService ?? repositoryMatch;
		selectedServiceId = resolvedSelection?.id ?? null;

		if (project && resolvedSelection) {
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				project,
				resolvedSelection,
			);
		} else if (onboarding.projectId || onboarding.serviceId) {
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				project,
				undefined,
			);
		}

		reconciledDraft = {
			...reconciledDraft,
			environmentId: environment?.id ?? "",
		};
		if (!onboardingDraftEquals(onboarding, reconciledDraft)) {
			reconciledDraft = await saveOnboardingDraft(
				runtime,
				session.user.id,
				reconciledDraft,
			);
		}

		return {
			...baseState,
			canManageFleet: fleet !== undefined,
			onboarding: reconciledDraft,
			project,
			environments,
			environment,
			services: allServices,
			servicesRevision,
			selectedServiceId,
		} satisfies DashboardHomeState;
	} catch (error) {
		if (error instanceof PlatformGatewayError) {
			return {
				...baseState,
				controlPlaneReachable: false,
				controlPlaneError: error.message,
			} satisfies DashboardHomeState;
		}
		throw error;
	}
}
