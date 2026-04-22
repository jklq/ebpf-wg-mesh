import {
	currentSession,
	requireSession,
} from "#/lib/dashboard/core/auth.server";
import {
	buildHealthyAndReady,
	type DashboardRuntime,
	listGitHubRepositories,
	loadOnboardingDraft,
	nextServiceName,
	onboardingDraftEquals,
	parseContainerPort,
	platformCall,
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
	type DashboardDomainBinding,
	type DashboardHomeState,
	type DashboardOnboardingDraft,
	type DashboardProject,
	type DashboardServiceRecord,
	type DashboardServiceStatus,
	type DashboardSourceSpec,
	DashboardValidationError,
	GitHubApiError,
	type GitHubUserRepository,
	PlatformGatewayError,
	type UpdateServiceInput,
} from "#/lib/dashboard/core/types.server";
import type { DomainVerificationResult } from "#/lib/dashboard/domain/dns.server";
import { normalizeHostname } from "#/lib/dashboard/domain/dns.server";
import { normalizeRepositorySelector } from "#/lib/dashboard/onboarding/flow";

export async function loadDashboardHome(
	runtime: DashboardRuntime,
): Promise<DashboardHomeState | null> {
	const session = await currentSession(runtime);
	if (!session) {
		return null;
	}

	const { config } = runtime;
	await storeCall(runtime, "ensureSessionUser", (store) =>
		store.ensureSessionUser(session.user),
	);
	const onboarding = await loadOnboardingDraft(runtime, session.user.id);
	let githubAccount = await storeCall(runtime, "getGitHubAccount", (store) =>
		store.getGitHubAccount(session.user.id),
	);
	let repositories: Array<GitHubUserRepository> = [];
	if (githubAccount?.accessToken) {
		try {
			repositories = await listGitHubRepositories(
				runtime,
				githubAccount.accessToken,
			);
		} catch (cause) {
			const err = toGitHubApiError("listRepositories", cause);
			if (
				err instanceof GitHubApiError &&
				err.status === 401 &&
				githubAccount.refreshToken
			) {
				try {
					githubAccount = await refreshGitHubAccount(runtime, githubAccount);
					repositories = await listGitHubRepositories(
						runtime,
						githubAccount.accessToken,
					);
				} catch {
					repositories = [];
				}
			}
		}
	}

	const baseState = {
		user: session.user,
		githubAccount: githubAccount ?? undefined,
		onboarding,
		repositories,
		githubLoginURL: runtime.github ? "/auth/start?redirect=%2F" : undefined,
		githubInstallURL: config.githubInstallURL,
		publicBaseURL: config.publicBaseURL,
		localIngressBaseURL: config.localIngressBaseURL,
		ingressTargetHost: config.ingressTargetHost,
		localDomainSuffix: config.localDomainSuffix,
		services: [],
		domainBindings: [],
		controlPlaneReachable: true,
	} satisfies DashboardHomeState;

	try {
		await platformCall(runtime, "ensurePrincipal", (platform) =>
			platform.ensurePrincipal(session.user),
		);
		const projects = await platformCall(runtime, "listProjects", (platform) =>
			platform.listProjects(session.user),
		);
		let project = onboarding.projectId
			? projects.find((entry) => entry.id === onboarding.projectId)
			: undefined;
		if (!project && onboarding.repositorySelector) {
			project = projects.find(
				(entry) => entry.name === onboarding.repositorySelector,
			);
		}
		const repositoryInspection = onboarding.repositorySelector
			? await platformCall(runtime, "inspectRepositorySource", (platform) =>
					platform.inspectRepositorySource(session.user, {
						provider: "github",
						repositorySelector: onboarding.repositorySelector,
					}),
				)
			: undefined;

		let reconciledDraft = onboarding;
		let allServices: Array<DashboardServiceRecord> = [];
		let service: DashboardServiceRecord | undefined;
		let serviceStatus: DashboardServiceStatus | undefined;
		let domainBindings: Array<DashboardDomainBinding> = [];

		if (project) {
			allServices =
				(await safePlatformCall(runtime, "listServices", (platform) =>
					platform.listServices(session.user, project.id),
				)) ?? [];
		}

		if (project && onboarding.serviceId) {
			service = allServices.find((s) => s.id === onboarding.serviceId);
			if (!service) {
				service = await safePlatformCall(runtime, "getService", (platform) =>
					platform.getService(session.user, {
						projectId: project.id,
						serviceId: onboarding.serviceId,
					}),
				);
			}
		}

		if (!service && project && onboarding.repositorySelector) {
			service = allServices.find(
				(entry) =>
					entry.spec?.repositorySelector === onboarding.repositorySelector,
			);
		}

		if (project && service) {
			serviceStatus = await safePlatformCall(
				runtime,
				"getServiceStatus",
				(platform) =>
					platform.getServiceStatus(session.user, {
						projectId: project.id,
						serviceId: service.id,
					}),
			);
			domainBindings =
				(await safePlatformCall(runtime, "listDomainBindings", (platform) =>
					platform.listDomainBindings(session.user, {
						projectId: project.id,
						serviceId: service.id,
					}),
				)) ?? [];
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				repositoryInspection,
				project,
				service,
				serviceStatus,
				domainBindings,
			);
		} else if (onboarding.projectId || onboarding.serviceId) {
			reconciledDraft = reconcileOnboardingDraft(
				onboarding,
				repositoryInspection,
				project,
				undefined,
				undefined,
				[],
			);
		}

		if (!onboardingDraftEquals(onboarding, reconciledDraft)) {
			reconciledDraft = await saveOnboardingDraft(
				runtime,
				session.user.id,
				reconciledDraft,
			);
		}

		const domainVerification = reconciledDraft.hostname
			? await safeVerifyHostname(runtime, reconciledDraft.hostname)
			: undefined;

		return {
			...baseState,
			onboarding: reconciledDraft,
			project,
			services: allServices,
			service,
			serviceStatus,
			repositoryInspection,
			domainVerification,
			domainBindings,
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
	await platformCall(runtime, "ensurePrincipal", (platform) =>
		platform.ensurePrincipal(session.user),
	);
	return platformCall(runtime, "createProject", (platform) =>
		platform.createProject(session.user, projectName),
	);
}

export async function inspectRepositoryFromSession(
	runtime: DashboardRuntime,
	input: { repositorySelector: string },
): Promise<DashboardOnboardingDraft> {
	const session = await requireSession(runtime);
	const selector = normalizeRepositorySelector(input.repositorySelector);
	await platformCall(runtime, "ensurePrincipal", (platform) =>
		platform.ensurePrincipal(session.user),
	);
	const inspection = await platformCall(
		runtime,
		"inspectRepositorySource",
		(platform) =>
			platform.inspectRepositorySource(session.user, {
				provider: "github",
				repositorySelector: selector,
			}),
	);
	const draft = await loadOnboardingDraft(runtime, session.user.id);
	const recommended = inspection.recommendedBuildRecipe;
	const selectorChanged = draft.repositorySelector !== selector;
	const nextDraft: DashboardOnboardingDraft = {
		...draft,
		currentStep: "repository",
		projectId: selectorChanged ? "" : draft.projectId,
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
		containerPort: selectorChanged ? "" : draft.containerPort,
		hostname: selectorChanged ? "" : draft.hostname,
	};
	return saveOnboardingDraft(runtime, session.user.id, nextDraft);
}

export async function confirmRepositoryFromSession(
	runtime: DashboardRuntime,
	input: {
		repositorySelector: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
		containerPort?: string;
	},
): Promise<DashboardOnboardingDraft> {
	const session = await requireSession(runtime);
	const selector = normalizeRepositorySelector(input.repositorySelector);
	await platformCall(runtime, "ensurePrincipal", (platform) =>
		platform.ensurePrincipal(session.user),
	);
	const inspection = await platformCall(
		runtime,
		"inspectRepositorySource",
		(platform) =>
			platform.inspectRepositorySource(session.user, {
				provider: "github",
				repositorySelector: selector,
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
	if (dockerfilePath === "") {
		throw new DashboardValidationError({
			message:
				"No Dockerfile was detected for this repository. Pick a repo with a Dockerfile or add one first.",
		});
	}
	const contextDir =
		input.contextDir?.trim() ||
		inspection.recommendedBuildRecipe?.contextDir ||
		".";
	const containerPort = parseContainerPort(input.containerPort);
	const trackedRef =
		input.trackedRef?.trim() || inspection.defaultBranch || "main";
	const projects = await platformCall(runtime, "listProjects", (platform) =>
		platform.listProjects(session.user),
	);
	const projectName = selector;
	const project =
		projects.find((entry) => entry.name === projectName) ??
		(await platformCall(runtime, "createProject", (platform) =>
			platform.createProject(session.user, projectName),
		));
	const services = await platformCall(runtime, "listServices", (platform) =>
		platform.listServices(session.user, project.id),
	);
	const existingService = services.find(
		(entry) => entry.spec?.repositorySelector === selector,
	);
	const desiredSource: DashboardSourceSpec = {
		provider: "github",
		repositorySelector: selector,
		trackedRef,
		buildRecipe: {
			dockerfilePath,
			contextDir,
		},
		containerPort,
	};
	const service = existingService
		? await platformCall(runtime, "updateService", (platform) =>
				platform.updateService(session.user, {
					projectId: project.id,
					serviceId: existingService.id,
					source: desiredSource,
				}),
			)
		: await platformCall(runtime, "createService", (platform) =>
				platform.createService(session.user, {
					projectId: project.id,
					name: nextServiceName(services, selector),
					source: desiredSource,
				}),
			);
	return saveOnboardingDraft(runtime, session.user.id, {
		currentStep: "build",
		projectId: project.id,
		serviceId: service.id,
		repositorySelector: selector,
		trackedRef,
		dockerfilePath,
		contextDir,
		containerPort: String(containerPort),
		hostname: "",
	});
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
	const serviceStatus = await platformCall(
		runtime,
		"getServiceStatus",
		(platform) =>
			platform.getServiceStatus(session.user, {
				projectId: draft.projectId,
				serviceId: draft.serviceId,
			}),
	);
	if (!buildHealthyAndReady(serviceStatus)) {
		throw new DashboardValidationError({
			message:
				"Wait for the latest build to succeed and the deployment to become healthy before publishing a domain.",
		});
	}
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
				projectId: draft.projectId,
				serviceId: draft.serviceId,
				hostname: draft.hostname,
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
	input: { projectId: string; serviceId: string },
): Promise<DashboardServiceStatus> {
	const session = await requireSession(runtime);
	return platformCall(runtime, "getServiceStatus", (platform) =>
		platform.getServiceStatus(session.user, input),
	);
}

export async function updateServiceFromSession(
	runtime: DashboardRuntime,
	input: UpdateServiceInput,
): Promise<DashboardServiceRecord> {
	const session = await requireSession(runtime);
	const containerPort = parseContainerPort(input.containerPort);
	const desiredSource: DashboardSourceSpec = {
		provider: "github",
		repositorySelector: normalizeRepositorySelector(input.repositorySelector),
		trackedRef: input.trackedRef.trim() || "main",
		buildRecipe: {
			dockerfilePath: input.dockerfilePath.trim(),
			contextDir: input.contextDir.trim() || ".",
		},
		containerPort,
	};
	return platformCall(runtime, "updateService", (platform) =>
		platform.updateService(session.user, {
			projectId: input.projectId,
			serviceId: input.serviceId,
			source: desiredSource,
		}),
	);
}

export async function listDomainBindingsFromSession(
	runtime: DashboardRuntime,
	input: { projectId: string; serviceId: string },
): Promise<Array<DashboardDomainBinding>> {
	const session = await requireSession(runtime);
	return (
		(await safePlatformCall(runtime, "listDomainBindings", (platform) =>
			platform.listDomainBindings(session.user, input),
		)) ?? []
	);
}

export async function createDomainBindingFromSession(
	runtime: DashboardRuntime,
	input: { projectId: string; serviceId: string; hostname: string },
): Promise<DashboardDomainBinding> {
	const session = await requireSession(runtime);
	const normalizedHostname = normalizeHostname(input.hostname);
	const verification = await verifyHostnameOrThrow(runtime, normalizedHostname);
	if (verification.state !== "verified") {
		throw new DashboardValidationError({
			message: "DNS has not verified yet for this hostname.",
		});
	}
	return platformCall(runtime, "createDomainBinding", (platform) =>
		platform.createDomainBinding(session.user, {
			projectId: input.projectId,
			serviceId: input.serviceId,
			hostname: normalizedHostname,
		}),
	);
}

export async function checkDomainDNSFromSession(
	runtime: DashboardRuntime,
	hostname: string,
): Promise<DomainVerificationResult | undefined> {
	await requireSession(runtime);
	return safeVerifyHostname(runtime, normalizeHostname(hostname));
}
