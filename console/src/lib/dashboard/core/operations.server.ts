import {
	currentSession,
	requireSession,
} from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	listGitHubRepositories,
	loadOnboardingDraft,
	nextGeneratedProjectName,
	nextGeneratedServiceName,
	onboardingDraftEquals,
	parseTargetPort,
	platformCall,
	recommendedTargetPort,
	reconcileOnboardingDraft,
	refreshGitHubAccount,
	safePlatformCall,
	safeVerifyHostname,
	saveOnboardingDraft,
	storeCall,
	toGitHubApiError,
	verifyHostnameOrThrow,
} from "#/lib/dashboard/core/runtime.server";
import {
	type CreateServiceFastResult,
	type DashboardAgentEnrollment,
	type DashboardAgentLifecycleState,
	type DashboardDeploymentAction,
	type DashboardDeploymentRecord,
	type DashboardDomainBinding,
	type DashboardEnvironment,
	type DashboardFleet,
	type DashboardFleetAgent,
	type DashboardGitHubAccount,
	type DashboardHomeState,
	type DashboardOnboardingDraft,
	type DashboardProject,
	type DashboardRepositoryInspection,
	type DashboardServiceLogLine,
	type DashboardServiceLogType,
	type DashboardServicePosition,
	type DashboardServiceRecord,
	type DashboardServiceSpec,
	type DashboardServiceStatus,
	type DashboardSourceSpec,
	type DashboardUser,
	DashboardValidationError,
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
	type FleetAgentInput,
	GitHubApiError,
	type GitHubUserRepository,
	PlatformGatewayError,
	type StoredDashboardGitHubAccount,
	type UpdateServiceInput,
} from "#/lib/dashboard/core/types.server";
import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";
import { normalizeHostname } from "#/lib/dashboard/domain/dns.server";
import { normalizeRepositorySelector } from "#/lib/dashboard/onboarding/flow";

export async function loadFleetFromSession(
	runtime: DashboardRuntime,
): Promise<DashboardFleet> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "listFleet", (platform) =>
		platform.listFleet(session.user),
	);
}

export async function createFleetAgentFromSession(
	runtime: DashboardRuntime,
	input: FleetAgentInput,
): Promise<DashboardAgentEnrollment> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "createFleetAgent", (platform) =>
		platform.createFleetAgent(session.user, normalizeFleetAgentInput(input)),
	);
}

export async function updateFleetAgentFromSession(
	runtime: DashboardRuntime,
	input: FleetAgentInput,
): Promise<DashboardFleetAgent> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "updateFleetAgent", (platform) =>
		platform.updateFleetAgent(session.user, normalizeFleetAgentInput(input)),
	);
}

export async function setFleetAgentLifecycleFromSession(
	runtime: DashboardRuntime,
	input: { agentId: string; lifecycleState: DashboardAgentLifecycleState },
): Promise<DashboardFleetAgent> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "setFleetAgentLifecycle", (platform) =>
		platform.setFleetAgentLifecycle(session.user, {
			agentId: input.agentId.trim(),
			lifecycleState: input.lifecycleState,
		}),
	);
}

function normalizeFleetAgentInput(input: FleetAgentInput): FleetAgentInput {
	return {
		agentId: input.agentId.trim(),
		name: input.name.trim(),
		region: input.region.trim().toLowerCase(),
		zone: input.zone.trim().toLowerCase(),
		failureDomain: input.failureDomain.trim().toLowerCase(),
		reservedCpuMillis: Number(input.reservedCpuMillis),
		reservedMemoryMebibytes: Number(input.reservedMemoryMebibytes),
	};
}

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
		domainBindings: [],
		controlPlaneReachable: true,
	} satisfies DashboardHomeState;

	try {
		const projects = await platformCall(runtime, "listProjects", (platform) =>
			platform.listProjects(session.user),
		);
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
		let service: DashboardServiceRecord | undefined;

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
			const [servicesResult, positions] = await Promise.all([
				safePlatformCall(runtime, "listServices", (platform) =>
					platform.listServices(session.user, environment.id),
				),
				storeCall(runtime, "listServicePositions", (store) =>
					store.listServicePositions(session.user.id, environment.id),
				),
			]);
			allServices = servicesResult ?? [];
			allServices = applyServicePositions(allServices, positions);
		}

		if (project && onboarding.serviceId) {
			service = allServices.find((s) => s.id === onboarding.serviceId);
		}

		if (!service && project && onboarding.repositorySelector) {
			const matchingServices = allServices.filter(
				(entry) =>
					entry.spec?.source?.repositorySelector ===
					onboarding.repositorySelector,
			);
			if (matchingServices.length === 1) {
				service = matchingServices[0];
			}
		}

		if (project && service) {
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				undefined,
				project,
				service,
				undefined,
				[],
			);
		} else if (onboarding.projectId || onboarding.serviceId) {
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				undefined,
				project,
				undefined,
				undefined,
				[],
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
			onboarding: reconciledDraft,
			project,
			environments,
			environment,
			services: allServices,
			service,
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

export async function loadGitHubCatalogFromSession(
	runtime: DashboardRuntime,
): Promise<{
	githubAccount?: DashboardGitHubAccount;
	repositories: Array<GitHubUserRepository>;
}> {
	const session = await requireSession(runtime);
	const githubAccount = await storeCall(runtime, "getGitHubAccount", (store) =>
		store.getGitHubAccount(session.user.id),
	);
	const catalog = await loadGitHubCatalog(
		runtime,
		session.user.id,
		githubAccount,
	);
	return {
		githubAccount: publicGitHubAccount(catalog.githubAccount),
		repositories: catalog.repositories,
	};
}

async function loadGitHubCatalog(
	runtime: DashboardRuntime,
	userID: string,
	githubAccount: StoredDashboardGitHubAccount | null,
): Promise<{
	githubAccount: StoredDashboardGitHubAccount | null;
	repositories: Array<GitHubUserRepository>;
}> {
	if (!githubAccount?.accessToken) {
		return { githubAccount, repositories: [] };
	}
	try {
		return {
			githubAccount,
			repositories: await listGitHubRepositories(
				runtime,
				githubAccount.accessToken,
			),
		};
	} catch (cause) {
		const err = toGitHubApiError("listRepositories", cause);
		if (
			err instanceof GitHubApiError &&
			err.status === 401 &&
			githubAccount.refreshToken
		) {
			try {
				const refreshed = await refreshGitHubAccount(
					runtime,
					userID,
					githubAccount,
				);
				return {
					githubAccount: refreshed,
					repositories: await listGitHubRepositories(
						runtime,
						refreshed.accessToken,
					),
				};
			} catch {
				return { githubAccount, repositories: [] };
			}
		}
		return { githubAccount, repositories: [] };
	}
}

function publicGitHubAccount(
	account: StoredDashboardGitHubAccount | null,
): DashboardGitHubAccount | undefined {
	if (!account) {
		return undefined;
	}
	return {
		providerSubject: account.providerSubject,
		login: account.login,
		primaryEmail: account.primaryEmail,
		tokenType: account.tokenType,
		scope: account.scope,
	};
}

export async function inspectRepositorySourceFromSession(
	runtime: DashboardRuntime,
	input: { repositorySelector: string },
): Promise<DashboardRepositoryInspection | undefined> {
	const session = await requireSession(runtime);
	const selector = normalizeRepositorySelector(input.repositorySelector);
	if (!selector) return undefined;
	const githubUserAccessToken = await requireGitHubRepositoryAccess(
		runtime,
		session.user.id,
		selector,
	);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
	const project = await repositoryProject(
		runtime,
		session.user,
		draft.projectId,
	);
	return platformCall(runtime, "linkGitHubRepository", (platform) =>
		platform.linkGitHubRepository(session.user, {
			projectId: project.id,
			repositorySelector: selector,
			githubUserAccessToken,
		}),
	);
}

export async function createProjectFromSession(
	runtime: DashboardRuntime,
	name: string,
): Promise<DashboardProject> {
	const session = await requireSession(runtime);
	const projectName = name.trim();
	if (projectName === "") {
		throw new DashboardValidationError({
			message: "project name is required",
		});
	}
	return platformCall(runtime, "createProject", (platform) =>
		platform.createProject(session.user, projectName),
	);
}

export async function createEnvironmentFromSession(
	runtime: DashboardRuntime,
	input: { projectId: string; name: string },
): Promise<DashboardEnvironment> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "createEnvironment", (platform) =>
		platform.createEnvironment(session.user, {
			...input,
			name: input.name.trim(),
		}),
	);
}

export async function duplicateEnvironmentFromSession(
	runtime: DashboardRuntime,
	input: { sourceEnvironmentId: string; name: string; copyVariables: boolean },
): Promise<DashboardEnvironment> {
	const session = await requireSession(runtime);
	const duplicate = await platformCall(
		runtime,
		"duplicateEnvironment",
		(platform) =>
			platform.duplicateEnvironment(session.user, {
				...input,
				name: input.name.trim(),
			}),
	);
	const [sourceServices, copiedServices, sourcePositions] = await Promise.all([
		platformCall(runtime, "listServices", (platform) =>
			platform.listServices(session.user, input.sourceEnvironmentId),
		),
		platformCall(runtime, "listServices", (platform) =>
			platform.listServices(session.user, duplicate.id),
		),
		storeCall(runtime, "listServicePositions", (store) =>
			store.listServicePositions(session.user.id, input.sourceEnvironmentId),
		),
	]);
	const sourceByName = new Map(
		sourceServices.map((service) => [service.name, service]),
	);
	await Promise.all(
		copiedServices.map(async (service) => {
			const source = sourceByName.get(service.name);
			const position = source ? sourcePositions[source.id] : undefined;
			if (!position) return;
			await storeCall(runtime, "saveServicePosition", (store) =>
				store.saveServicePosition(session.user.id, {
					environmentId: duplicate.id,
					serviceId: service.id,
					position,
				}),
			);
		}),
	);
	return duplicate;
}

export async function renameEnvironmentFromSession(
	runtime: DashboardRuntime,
	input: { environmentId: string; name: string },
): Promise<DashboardEnvironment> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "renameEnvironment", (platform) =>
		platform.renameEnvironment(session.user, {
			...input,
			name: input.name.trim(),
		}),
	);
}

export async function deleteEnvironmentFromSession(
	runtime: DashboardRuntime,
	environmentId: string,
): Promise<void> {
	const session = await requireSession(runtime);
	await platformCall(runtime, "deleteEnvironment", (platform) =>
		platform.deleteEnvironment(session.user, environmentId),
	);
}

export async function deployEnvironmentFromSession(
	runtime: DashboardRuntime,
	environmentId: string,
): Promise<Array<DashboardServiceStatus>> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "deployEnvironment", (platform) =>
		platform.deployEnvironment(session.user, environmentId),
	);
}

export async function inspectRepositoryFromSession(
	runtime: DashboardRuntime,
	input: { repositorySelector: string },
): Promise<DashboardOnboardingDraft> {
	const session = await requireSession(runtime);
	const selector = normalizeRepositorySelector(input.repositorySelector);
	const githubUserAccessToken = await requireGitHubRepositoryAccess(
		runtime,
		session.user.id,
		selector,
	);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
	const project = await repositoryProject(
		runtime,
		session.user,
		draft.projectId,
	);
	const inspection = await platformCall(
		runtime,
		"linkGitHubRepository",
		(platform) =>
			platform.linkGitHubRepository(session.user, {
				projectId: project.id,
				repositorySelector: selector,
				githubUserAccessToken,
			}),
	);
	const recommended = inspection.recommendedBuildRecipe;
	const selectorChanged = draft.repositorySelector !== selector;
	const nextDraft: DashboardOnboardingDraft = {
		...draft,
		currentStep: "repository",
		projectId: project.id,
		serviceId: selectorChanged ? "" : draft.serviceId,
		repositorySelector: selector,
		trackedRef:
			selectorChanged || draft.trackedRef === ""
				? inspection.defaultBranch
				: draft.trackedRef,
		dockerfilePath:
			selectorChanged || draft.dockerfilePath === ""
				? (recommended?.dockerfilePath ?? "")
				: draft.dockerfilePath,
		contextDir:
			selectorChanged || draft.contextDir === ""
				? (recommended?.contextDir ?? "")
				: draft.contextDir,
		hostname: selectorChanged ? "" : draft.hostname,
	};
	return saveOnboardingDraft(runtime, session.user.id, nextDraft);
}

export async function confirmRepositoryFromSession(
	runtime: DashboardRuntime,
	input: {
		repositorySelector: string;
		serviceName?: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		cpuMillis?: number;
		memoryMebibytes?: number;
	},
): Promise<DashboardOnboardingDraft> {
	const result = await createServiceFastFromSession(runtime, input);
	return result.onboarding;
}

export async function createServiceFastFromSession(
	runtime: DashboardRuntime,
	input: {
		repositorySelector: string;
		serviceName?: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		cpuMillis?: number;
		memoryMebibytes?: number;
	},
): Promise<CreateServiceFastResult> {
	const session = await requireSession(runtime);
	const selector = normalizeRepositorySelector(input.repositorySelector);
	const githubUserAccessToken = await requireGitHubRepositoryAccess(
		runtime,
		session.user.id,
		selector,
	);
	const currentDraft = await loadOnboardingDraft(runtime, session.user.id);
	const projects = await platformCall(runtime, "listProjects", (platform) =>
		platform.listProjects(session.user),
	);
	const project =
		(currentDraft.projectId
			? projects.find((entry) => entry.id === currentDraft.projectId)
			: undefined) ??
		(await platformCall(runtime, "createProject", (platform) =>
			platform.createProject(
				session.user,
				nextGeneratedProjectName(projects, runtime.randomUUID()),
			),
		));
	const environments = await platformCall(
		runtime,
		"listEnvironments",
		(platform) => platform.listEnvironments(session.user, project.id),
	);
	const environment =
		environments.find((entry) => entry.id === currentDraft.environmentId) ??
		environments.find((entry) => entry.isProduction) ??
		environments[0];
	if (!environment) {
		throw new DashboardValidationError({
			message: "Project has no production environment.",
		});
	}
	const inspection = await platformCall(
		runtime,
		"linkGitHubRepository",
		(platform) =>
			platform.linkGitHubRepository(session.user, {
				projectId: project.id,
				repositorySelector: selector,
				githubUserAccessToken,
			}),
	);
	if (inspection.accessState !== "available") {
		throw new DashboardValidationError({
			message: "Repository access is not available yet.",
		});
	}

	const dockerfilePath =
		input.dockerfilePath?.trim() ||
		inspection.recommendedBuildRecipe?.dockerfilePath ||
		"";
	const contextDir =
		input.contextDir?.trim() ||
		inspection.recommendedBuildRecipe?.contextDir ||
		".";
	const trackedRef =
		input.trackedRef?.trim() || inspection.defaultBranch || "main";
	const services = await platformCall(runtime, "listServices", (platform) =>
		platform.listServices(session.user, environment.id),
	);
	const desiredSpec: DashboardServiceSpec = buildServiceSpec(
		{
			provider: "github",
			repositorySelector: selector,
			trackedRef,
			buildRecipe: {
				dockerfilePath,
				contextDir,
			},
		},
		inspection.recommendedPorts,
		input.cpuMillis,
		input.memoryMebibytes,
	);
	const service = await platformCall(runtime, "createService", (platform) =>
		platform.createService(session.user, {
			environmentId: environment.id,
			name: nextGeneratedServiceName(
				services,
				input.serviceName,
				runtime.randomUUID(),
			),
			spec: desiredSpec,
		}),
	);
	const onboarding = await saveOnboardingDraft(runtime, session.user.id, {
		currentStep: "build",
		projectId: project.id,
		environmentId: environment.id,
		serviceId: service.id,
		repositorySelector: selector,
		trackedRef,
		dockerfilePath,
		contextDir,
		hostname: "",
	});
	const serviceStatus =
		(await safePlatformCall(runtime, "getServiceStatus", (platform) =>
			platform.getServiceStatus(session.user, {
				serviceId: service.id,
			}),
		)) ?? null;
	return {
		project,
		environment,
		service,
		serviceStatus,
		onboarding,
	};
}

export async function saveHostnameFromSession(
	runtime: DashboardRuntime,
	hostname: string,
): Promise<DashboardOnboardingDraft> {
	const session = await requireSession(runtime);
	const normalizedHostname = normalizeHostname(hostname);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
	if (!draft.projectId || !draft.serviceId) {
		throw new DashboardValidationError({
			message: "Create a service before connecting a domain.",
		});
	}
	return saveOnboardingDraft(runtime, session.user.id, {
		...draft,
		currentStep: "domain",
		hostname: normalizedHostname,
	});
}

export async function publishDomainFromSession(
	runtime: DashboardRuntime,
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
	if (!draft.projectId || !draft.serviceId) {
		throw new DashboardValidationError({
			message: "Create a service before publishing a domain.",
		});
	}
	if (!draft.hostname) {
		throw new DashboardValidationError({
			message: "Enter a hostname first.",
		});
	}
	const service = await platformCall(runtime, "getService", (platform) =>
		platform.getService(session.user, {
			serviceId: draft.serviceId,
		}),
	);
	const serviceStatus = await safePlatformCall(
		runtime,
		"getServiceStatus",
		(platform) =>
			platform.getServiceStatus(session.user, {
				serviceId: draft.serviceId,
			}),
	);
	const verification = await verifyHostnameOrThrow(runtime, draft.hostname);
	if (verification.state !== "verified") {
		throw new DashboardValidationError({
			message: "DNS has not verified yet for this hostname.",
		});
	}
	const binding = await platformCall(
		runtime,
		"createDomainBinding",
		(platform) =>
			platform.createDomainBinding(session.user, {
				serviceId: draft.serviceId,
				hostname: draft.hostname,
				targetPort: recommendedTargetPort(service, serviceStatus),
			}),
	);
	await saveOnboardingDraft(runtime, session.user.id, {
		...draft,
		currentStep: "domain",
	});
	return binding;
}

export async function getServiceStatusFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<DashboardServiceStatus> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "getServiceStatus", (platform) =>
		platform.getServiceStatus(session.user, { serviceId: input.serviceId }),
	);
}

export async function waitForServiceStatusFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	return platformCall(runtime, "waitForServiceStatus", (platform) =>
		platform.waitForServiceStatus(session.user, input),
	);
}

export async function listEnvironmentServicesFromSession(
	runtime: DashboardRuntime,
	input: { environmentId: string },
): Promise<Array<DashboardServiceRecord>> {
	const session = await requireSession(runtime);
	const [services, positions] = await Promise.all([
		platformCall(runtime, "listServices", (platform) =>
			platform.listServices(session.user, input.environmentId),
		),
		storeCall(runtime, "listServicePositions", (store) =>
			store.listServicePositions(session.user.id, input.environmentId),
		),
	]);
	return applyServicePositions(services, positions);
}

export async function waitForEnvironmentServicesFromSession(
	runtime: DashboardRuntime,
	input: {
		environmentId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	const result = await platformCall(runtime, "waitForServices", (platform) =>
		platform.waitForServices(session.user, input),
	);
	if (result.notModified || !result.services) {
		return result;
	}
	const positions = await storeCall(runtime, "listServicePositions", (store) =>
		store.listServicePositions(session.user.id, input.environmentId),
	);
	return {
		...result,
		services: applyServicePositions(result.services, positions),
	};
}

export async function waitForProjectServicesFromSession(
	runtime: DashboardRuntime,
	input: {
		projectId: string;
		waitIndex: number;
		waitTimeoutSeconds: number;
	},
) {
	const session = await requireSession(runtime);
	const environments = await platformCall(
		runtime,
		"listEnvironments",
		(platform) => platform.listEnvironments(session.user, input.projectId),
	);
	const environment =
		environments.find((entry) => entry.isProduction) ?? environments[0];
	if (!environment) {
		throw new DashboardValidationError({
			message: "project has no environment",
		});
	}
	const result = await platformCall(runtime, "waitForServices", (platform) =>
		platform.waitForServices(session.user, {
			environmentId: environment.id,
			waitIndex: input.waitIndex,
			waitTimeoutSeconds: input.waitTimeoutSeconds,
		}),
	);
	if (result.notModified || !result.services) {
		return result;
	}
	const positions = await storeCall(runtime, "listServicePositions", (store) =>
		store.listServicePositions(session.user.id, environment.id),
	);
	return {
		...result,
		services: applyServicePositions(result.services, positions),
	};
}

export async function listServiceLogsFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		allocationId?: string;
		limit?: number;
		logType?: DashboardServiceLogType;
		buildId?: string;
		search?: string;
		startTime?: Date;
		endTime?: Date;
	},
): Promise<Array<DashboardServiceLogLine>> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "listServiceLogs", (platform) =>
		platform.listServiceLogs(session.user, input),
	);
}

export async function listServiceDeploymentsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string; limit?: number },
): Promise<Array<DashboardDeploymentRecord>> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "listServiceDeployments", (platform) =>
		platform.listServiceDeployments(session.user, {
			serviceId: input.serviceId,
			limit: input.limit,
		}),
	);
}

export async function updateServiceFromSession(
	runtime: DashboardRuntime,
	input: UpdateServiceInput,
): Promise<DashboardServiceRecord> {
	const session = await requireSession(runtime);
	const current = await platformCall(runtime, "getService", (platform) =>
		platform.getService(session.user, {
			serviceId: input.serviceId,
		}),
	);
	const currentSource = current.spec?.source;
	const repositorySelector =
		input.repositorySelector ?? currentSource?.repositorySelector ?? "";
	const trackedRef = input.trackedRef ?? currentSource?.trackedRef ?? "";
	const dockerfilePath =
		input.dockerfilePath ?? currentSource?.buildRecipe?.dockerfilePath ?? "";
	const contextDir =
		input.contextDir ?? currentSource?.buildRecipe?.contextDir ?? ".";
	const desiredSource: DashboardSourceSpec | undefined =
		repositorySelector.trim()
			? {
					provider: currentSource?.provider ?? "github",
					repositorySelector: normalizeRepositorySelector(repositorySelector),
					trackedRef: trackedRef.trim() || "main",
					buildRecipe: {
						dockerfilePath: dockerfilePath.trim(),
						contextDir: contextDir.trim() || ".",
					},
				}
			: undefined;
	const nextReplicaCount =
		input.desiredReplicaCount ?? current.spec?.desiredReplicaCount;
	if (nextReplicaCount !== undefined && nextReplicaCount < 1) {
		throw new DashboardValidationError({
			message: "Replica count must be at least 1.",
		});
	}
	if (desiredSource?.provider === "github") {
		const environment = await platformCall(
			runtime,
			"getEnvironment",
			(platform) =>
				platform.getEnvironment(session.user, current.environmentId),
		);
		const githubUserAccessToken = await requireGitHubRepositoryAccess(
			runtime,
			session.user.id,
			desiredSource.repositorySelector,
		);
		await platformCall(runtime, "linkGitHubRepository", (platform) =>
			platform.linkGitHubRepository(session.user, {
				projectId: environment.projectId,
				repositorySelector: desiredSource.repositorySelector,
				githubUserAccessToken,
			}),
		);
	}
	return platformCall(runtime, "updateService", (platform) =>
		platform.updateService(session.user, {
			serviceId: input.serviceId,
			...(input.serviceName?.trim() ? { name: input.serviceName.trim() } : {}),
			spec: {
				...(desiredSource ? { source: desiredSource } : {}),
				desiredReplicaCount:
					input.desiredReplicaCount ?? current.spec?.desiredReplicaCount,
				placementRegion:
					input.placementRegion?.trim().toLowerCase() ??
					current.spec?.placementRegion,
				rollingStrategy: input.rollingStrategy ?? current.spec?.rollingStrategy,
				runtime: {
					env: normalizeRuntimeEnv(
						input.runtimeEnv ?? current.spec?.runtime.env,
					),
					cpuMillis: normalizeResource(
						input.cpuMillis ?? current.spec?.runtime.cpuMillis,
						DEFAULT_SERVICE_CPU_MILLIS,
						"CPU request",
					),
					memoryMebibytes: normalizeResource(
						input.memoryMebibytes ?? current.spec?.runtime.memoryMebibytes,
						DEFAULT_SERVICE_MEMORY_MEBIBYTES,
						"memory request",
					),
					ports: current.spec?.runtime.ports ?? [],
					...(current.spec?.runtime.healthCheck
						? { healthCheck: current.spec.runtime.healthCheck }
						: {}),
					...(current.spec?.runtime.livenessCheck
						? { livenessCheck: current.spec.runtime.livenessCheck }
						: {}),
					...((input.restart ?? current.spec?.runtime.restart)
						? { restart: input.restart ?? current.spec?.runtime.restart }
						: {}),
					...(current.spec?.runtime.volumeName
						? { volumeName: current.spec.runtime.volumeName }
						: {}),
				},
			},
		}),
	);
}

export async function applyDeploymentActionFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		deploymentId: string;
		action: DashboardDeploymentAction;
		idempotencyKey: string;
		allocationId?: string;
	},
): Promise<DashboardServiceStatus> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "applyDeploymentAction", (platform) =>
		platform.applyDeploymentAction(session.user, input),
	);
}

export async function scaleServiceFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		desiredReplicaCount: number;
	},
): Promise<DashboardServiceStatus> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "scaleService", (platform) =>
		platform.scaleService(session.user, {
			serviceId: input.serviceId,
			desiredReplicaCount: input.desiredReplicaCount,
		}),
	);
}

export async function discardServiceChangesFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		changeIds?: Array<string>;
		discardAll?: boolean;
	},
): Promise<DashboardServiceRecord> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "discardServiceChanges", (platform) =>
		platform.discardServiceChanges(session.user, {
			serviceId: input.serviceId,
			changeIds: input.changeIds,
			discardAll: input.discardAll,
		}),
	);
}

export async function deleteServiceFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await platformCall(runtime, "deleteService", (platform) =>
		platform.deleteService(session.user, { serviceId: input.serviceId }),
	);
}

export async function saveServicePositionFromSession(
	runtime: DashboardRuntime,
	input: {
		environmentId: string;
		serviceId: string;
		position: DashboardServicePosition;
	},
): Promise<DashboardServicePosition> {
	const session = await requireSession(runtime);
	const position = normalizeServicePosition(input.position);
	return storeCall(runtime, "saveServicePosition", (store) =>
		store.saveServicePosition(session.user.id, {
			environmentId: input.environmentId,
			serviceId: input.serviceId,
			position,
		}),
	);
}

export async function listDomainBindingsFromSession(
	runtime: DashboardRuntime,
	input: { serviceId: string },
): Promise<Array<DashboardDomainBinding>> {
	const session = await requireSession(runtime);
	return (
		(await safePlatformCall(runtime, "listDomainBindings", (platform) =>
			platform.listDomainBindings(session.user, input),
		)) ?? []
	);
}

export async function generateDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		targetPort: string | number | undefined;
	},
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const targetPort = parseTargetPort(input.targetPort);
	return platformCall(runtime, "generateDomainBinding", (platform) =>
		platform.generateDomainBinding(session.user, {
			serviceId: input.serviceId,
			targetPort,
		}),
	);
}

export async function createDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		hostname: string;
		targetPort: string | number | undefined;
	},
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const normalizedHostname = normalizeHostname(input.hostname);
	const targetPort = parseTargetPort(input.targetPort);
	return platformCall(runtime, "createDomainBinding", (platform) =>
		platform.createDomainBinding(session.user, {
			serviceId: input.serviceId,
			hostname: normalizedHostname,
			targetPort,
		}),
	);
}

export async function updateDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: {
		serviceId: string;
		hostname: string;
		targetPort: string | number | undefined;
	},
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const normalizedHostname = normalizeHostname(input.hostname);
	const targetPort = parseTargetPort(input.targetPort);
	return platformCall(runtime, "updateDomainBinding", (platform) =>
		platform.updateDomainBinding(session.user, {
			hostname: normalizedHostname,
			serviceId: input.serviceId,
			targetPort,
		}),
	);
}

export async function deleteDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: { hostname: string },
): Promise<void> {
	const session = await requireSession(runtime);
	await platformCall(runtime, "deleteDomainBinding", (platform) =>
		platform.deleteDomainBinding(session.user, { hostname: input.hostname }),
	);
}

export async function checkDomainDNSFromSession(
	runtime: DashboardRuntime,
	hostname: string,
): Promise<DomainVerificationResult | undefined> {
	await requireSession(runtime);
	return safeVerifyHostname(runtime, normalizeHostname(hostname));
}

async function requireGitHubRepositoryAccess(
	runtime: DashboardRuntime,
	userID: string,
	repositorySelector: string,
): Promise<string> {
	const account = await storeCall(runtime, "getGitHubAccount", (store) =>
		store.getGitHubAccount(userID),
	);
	if (!account && runtime.config.devUsers.some((user) => user.id === userID)) {
		return "";
	}
	const catalog = await loadGitHubCatalog(runtime, userID, account);
	const expected = repositorySelector.toLowerCase();
	if (
		!catalog.repositories.some(
			(repository) => repository.fullName.toLowerCase() === expected,
		)
	) {
		throw new DashboardValidationError({
			message: "The signed-in GitHub account cannot access this repository.",
		});
	}
	return catalog.githubAccount?.accessToken ?? "";
}

async function repositoryProject(
	runtime: DashboardRuntime,
	user: DashboardUser,
	preferredProjectID: string,
): Promise<DashboardProject> {
	const projects = await platformCall(runtime, "listProjects", (platform) =>
		platform.listProjects(user),
	);
	const existing = projects.find(
		(project) => project.id === preferredProjectID,
	);
	if (existing) {
		return existing;
	}
	return platformCall(runtime, "createProject", (platform) =>
		platform.createProject(
			user,
			nextGeneratedProjectName(projects, runtime.randomUUID()),
		),
	);
}

function buildServiceSpec(
	source: DashboardSourceSpec,
	recommendedPorts: number[],
	cpuMillis?: number,
	memoryMebibytes?: number,
): DashboardServiceSpec {
	const seen = new Set<number>();
	const ports = recommendedPorts
		.filter((port) => Number.isInteger(port) && port >= 1 && port <= 65535)
		.filter((port) => {
			if (seen.has(port)) {
				return false;
			}
			seen.add(port);
			return true;
		})
		.map((port, index) => ({
			port,
			primary: index === 0,
		}));
	return {
		source,
		desiredReplicaCount: 1,
		runtime: {
			env: {},
			cpuMillis: normalizeResource(
				cpuMillis,
				DEFAULT_SERVICE_CPU_MILLIS,
				"CPU request",
			),
			memoryMebibytes: normalizeResource(
				memoryMebibytes,
				DEFAULT_SERVICE_MEMORY_MEBIBYTES,
				"memory request",
			),
			ports,
		},
	};
}

function normalizeResource(
	value: number | undefined,
	fallback: number,
	label: string,
): number {
	const normalized = value ?? fallback;
	if (!Number.isSafeInteger(normalized) || normalized < fallback) {
		throw new DashboardValidationError({
			message: `${label} must be at least ${fallback}`,
		});
	}
	return normalized;
}

function normalizeRuntimeEnv(
	env: Record<string, string> | undefined,
): Record<string, string> {
	if (!env) {
		return {};
	}
	const normalized: Record<string, string> = {};
	for (const [rawKey, rawValue] of Object.entries(env)) {
		const key = rawKey.trim();
		if (key === "") {
			continue;
		}
		if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
			throw new DashboardValidationError({
				message: `Invalid environment variable name: ${key}`,
			});
		}
		normalized[key] = String(rawValue);
	}
	return normalized;
}

function applyServicePositions(
	services: Array<DashboardServiceRecord>,
	positions: Record<string, DashboardServicePosition>,
): Array<DashboardServiceRecord> {
	return services.map((service) => {
		const position = positions[service.id];
		return position ? { ...service, layoutPosition: position } : service;
	});
}

function normalizeServicePosition(
	position: DashboardServicePosition,
): DashboardServicePosition {
	const x = Math.round(position.x);
	const y = Math.round(position.y);
	if (!Number.isFinite(x) || !Number.isFinite(y)) {
		throw new DashboardValidationError({
			message: "service position must be finite",
		});
	}
	return { x, y };
}
