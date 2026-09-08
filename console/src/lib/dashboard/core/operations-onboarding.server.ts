import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	loadOnboardingDraft,
	nextGeneratedProjectName,
	nextGeneratedServiceName,
	platformCall,
	safePlatformCall,
	saveOnboardingDraft,
	storeCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	type CreateServiceFastResult,
	type DashboardEnvironment,
	type DashboardGitHubAccount,
	type DashboardProject,
	type DashboardServiceSpec,
	type DashboardServiceStatus,
	DashboardValidationError,
	type GitHubUserRepository,
} from "#/lib/dashboard/core/types.server";
import { normalizeRepositorySelector } from "#/lib/dashboard/onboarding/flow";
import {
	buildServiceSpec,
	loadGitHubCatalog,
	publicGitHubAccount,
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
	const [sourceSnapshot, copiedSnapshot, sourcePositions] = await Promise.all([
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
		(sourceSnapshot.services ?? []).map((service) => [service.name, service]),
	);
	await Promise.all(
		(copiedSnapshot.services ?? []).map(async (service) => {
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
	const servicesSnapshot = await platformCall(
		runtime,
		"listServices",
		(platform) => platform.listServices(session.user, environment.id),
	);
	const services = servicesSnapshot.services ?? [];
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
