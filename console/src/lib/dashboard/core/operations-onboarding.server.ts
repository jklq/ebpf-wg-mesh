import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	loadOnboardingDraft,
	nextGeneratedProjectName,
	nextGeneratedServiceName,
	platformCall,
	recommendedTargetPort,
	safePlatformCall,
	saveOnboardingDraft,
	storeCall,
	verifyHostnameOrThrow,
} from "#/lib/dashboard/core/runtime.server";
import {
	type CreateServiceFastResult,
	type DashboardDomainBinding,
	type DashboardEnvironment,
	type DashboardGitHubAccount,
	type DashboardOnboardingDraft,
	type DashboardProject,
	type DashboardRepositoryInspection,
	type DashboardServiceSpec,
	type DashboardServiceStatus,
	DashboardValidationError,
	type GitHubUserRepository,
} from "#/lib/dashboard/core/types.server";
import { normalizeHostname } from "#/lib/dashboard/domain/dns.server";
import { normalizeRepositorySelector } from "#/lib/dashboard/onboarding/flow";
import {
	buildServiceSpec,
	loadGitHubCatalog,
	publicGitHubAccount,
	repositoryProject,
	requireGitHubRepositoryAccess,
} from "./operations-helpers.server";

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

export async function releaseEnvironmentFromSession(
	runtime: DashboardRuntime,
	environmentId: string,
): Promise<Array<DashboardServiceStatus>> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "releaseEnvironment", (platform) =>
		platform.releaseEnvironment(session.user, environmentId),
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
	if (inspection.accessState !== "SOURCE_ACCESS_STATE_AVAILABLE") {
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
