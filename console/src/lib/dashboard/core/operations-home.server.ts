import { currentSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	loadOnboardingDraft,
	onboardingDraftEquals,
	optionalPlatformResult,
	reconcileOnboardingDraft,
	saveOnboardingDraft,
	storeCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardHomeState,
	type DashboardServiceRecord,
	PlatformGatewayError,
} from "#/lib/dashboard/core/types.server";
import { OpsService, PlatformService } from "#/lib/platform-gen/platform_pb";
import {
	applyServicePositions,
	publicGitHubAccount,
} from "./operations-helpers.server";
import { listEnvironmentVolumes } from "./operations-volumes.server";

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
		projects: [],
		environments: [],
		services: [],
		volumes: [],
		servicesRevision: "0",
		selectedServiceId: null,
		domainBindings: [],
		controlPlaneReachable: true,
	} satisfies DashboardHomeState;

	try {
		const [projects, fleet] = await Promise.all([
			runtime.platform
				.call(PlatformService.method.listProjects, session.user, {})
				.then((response) => response.projects ?? []),
			optionalPlatformResult(
				runtime.platform.call(OpsService.method.listFleet, session.user, {}),
			),
		]);
		const selectedEnvironment = selectedEnvironmentId
			? await optionalPlatformResult(
					runtime.platform.call(
						PlatformService.method.getEnvironment,
						session.user,
						{ environmentId: selectedEnvironmentId },
					),
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
		if (!project && projects.length > 0) {
			project = projects[0];
		}

		let reconciledDraft = onboarding;
		let environments: DashboardHomeState["environments"] = [];
		let environment: DashboardHomeState["environment"];
		let allServices: Array<DashboardServiceRecord> = [];
		let volumes: DashboardHomeState["volumes"] = [];
		let servicesRevision = "0";
		let selectedServiceId: string | null = null;

		if (project) {
			environments = await runtime.platform
				.call(PlatformService.method.listEnvironments, session.user, {
					projectId: project.id,
				})
				.then((response) => response.environments ?? []);
			environment =
				environments.find((entry) => entry.id === selectedEnvironment?.id) ??
				environments.find((entry) => entry.id === onboarding.environmentId) ??
				environments.find((entry) => entry.isProduction) ??
				environments[0];
		}

		if (environment) {
			const [servicesSnapshot, positions, environmentVolumes] =
				await Promise.all([
					optionalPlatformResult(
						runtime.platform.call(
							PlatformService.method.listServices,
							session.user,
							{ environmentId: environment.id },
						),
					),
					storeCall(runtime, "listServicePositions", (store) =>
						store.listServicePositions(session.user.id, environment.id),
					),
					optionalPlatformResult(
						listEnvironmentVolumes(runtime, session.user, environment.id),
					),
				]);
			volumes = environmentVolumes ?? [];
			allServices = applyServicePositions(
				servicesSnapshot?.services ?? [],
				positions,
			);
			servicesRevision = servicesSnapshot?.index ?? "0";
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
								entry.spec?.source?.sourceSpec?.repositorySelector ===
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
			projects,
			environments,
			environment,
			services: allServices,
			volumes,
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
