import type {
	DashboardDeploymentRecord,
	DashboardDomainBinding,
	DashboardEnvironment,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardServiceLogLine,
	DashboardServiceLogType,
	DashboardServiceRecord,
	DashboardServiceSpec,
	DashboardServiceStatus,
	DashboardUser,
	PlatformGateway,
} from "#/lib/dashboard/core/types.server";

export interface FakePlatformGateway extends PlatformGateway {
	listProjectsCalls: Array<DashboardUser>;
	createProjectCalls: Array<{ user: DashboardUser; name: string }>;
	listEnvironmentsCalls: Array<{ user: DashboardUser; projectId: string }>;
	listServicesCalls: Array<{ user: DashboardUser; environmentId: string }>;
	inspectRepositorySourceCalls: Array<{
		user: DashboardUser;
		projectId: string;
		provider: string;
		repositorySelector: string;
		githubUserAccessToken: string;
	}>;
	linkGitHubRepositoryCalls: Array<{
		user: DashboardUser;
		projectId: string;
		repositorySelector: string;
		githubUserAccessToken: string;
	}>;
	createServiceCalls: Array<{
		user: DashboardUser;
		environmentId: string;
		name: string;
		spec: DashboardServiceSpec;
	}>;
	updateServiceCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		name?: string;
		spec: DashboardServiceSpec;
	}>;
	redeployServiceCalls: Array<{
		user: DashboardUser;
		serviceId: string;
	}>;
	scaleServiceCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		desiredReplicaCount: number;
	}>;
	restartServiceCalls: Array<{
		user: DashboardUser;
		serviceId: string;
	}>;
	discardServiceChangesCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		changeIds?: Array<string>;
		discardAll?: boolean;
	}>;
	deleteServiceCalls: Array<{
		user: DashboardUser;
		serviceId: string;
	}>;
	getServiceCalls: Array<{
		user: DashboardUser;
		serviceId: string;
	}>;
	getServiceStatusCalls: Array<{
		user: DashboardUser;
		serviceId: string;
	}>;
	listServiceLogsCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		allocationId?: string;
		limit?: number;
		logType?: DashboardServiceLogType;
		buildId?: string;
		search?: string;
		startTime?: Date;
		endTime?: Date;
	}>;
	listServiceDeploymentsCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		limit?: number;
	}>;
	listDomainBindingsCalls: Array<{
		user: DashboardUser;
		serviceId: string;
	}>;
	createDomainBindingCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		hostname: string;
		targetPort: number;
	}>;
	projects: Array<DashboardProject>;
	environments: Array<DashboardEnvironment>;
	services: Array<DashboardServiceRecord>;
	serviceStatuses: Map<string, DashboardServiceStatus>;
	serviceLogs: Array<DashboardServiceLogLine>;
	serviceDeployments: Array<DashboardDeploymentRecord>;
	domainBindings: Array<DashboardDomainBinding>;
	nextRepositoryInspection?: DashboardRepositoryInspection;
	errors: {
		listProjects?: Error;
		createProject?: Error;
		listServices?: Error;
		inspectRepositorySource?: Error;
		createService?: Error;
		updateService?: Error;
		redeployService?: Error;
		scaleService?: Error;
		restartService?: Error;
		discardServiceChanges?: Error;
		deleteService?: Error;
		getService?: Error;
		getServiceStatus?: Error;
		listServiceLogs?: Error;
		listServiceDeployments?: Error;
		listDomainBindings?: Error;
		createDomainBinding?: Error;
	};
}

export function createFakePlatformGateway(): FakePlatformGateway {
	const platform: FakePlatformGateway = {
		listProjectsCalls: [],
		createProjectCalls: [],
		listEnvironmentsCalls: [],
		listServicesCalls: [],
		inspectRepositorySourceCalls: [],
		linkGitHubRepositoryCalls: [],
		createServiceCalls: [],
		updateServiceCalls: [],
		redeployServiceCalls: [],
		scaleServiceCalls: [],
		restartServiceCalls: [],
		discardServiceChangesCalls: [],
		deleteServiceCalls: [],
		getServiceCalls: [],
		getServiceStatusCalls: [],
		listServiceLogsCalls: [],
		listServiceDeploymentsCalls: [],
		listDomainBindingsCalls: [],
		createDomainBindingCalls: [],
		projects: [],
		environments: [],
		services: [],
		serviceStatuses: new Map<string, DashboardServiceStatus>(),
		serviceLogs: [],
		serviceDeployments: [],
		domainBindings: [],
		nextRepositoryInspection: undefined,
		errors: {},
		async listFleet() {
			return {
				agents: [],
				capacity: {
					nodeCount: 0,
					schedulableNodeCount: 0,
					schedulableCpuMillis: 0,
					schedulableMemoryMebibytes: 0,
					allocatedCpuMillis: 0,
					allocatedMemoryMebibytes: 0,
					headroomCpuMillis: 0,
					headroomMemoryMebibytes: 0,
				},
			};
		},
		async createFleetAgent() {
			throw new Error("fleet enrollment is not available in tests");
		},
		async updateFleetAgent() {
			throw new Error("fleet updates are not available in tests");
		},
		async setFleetAgentLifecycle() {
			throw new Error("fleet lifecycle is not available in tests");
		},
		async listProjects(user): Promise<Array<DashboardProject>> {
			platform.listProjectsCalls.push(user);
			if (platform.errors.listProjects) {
				throw platform.errors.listProjects;
			}
			return platform.projects;
		},
		async createProject(user, name): Promise<DashboardProject> {
			platform.createProjectCalls.push({ user, name });
			if (platform.errors.createProject) {
				throw platform.errors.createProject;
			}
			const project: DashboardProject = {
				id: `project-${platform.createProjectCalls.length}`,
				name,
				kind: "user",
			};
			platform.projects = [...platform.projects, project];
			platform.environments = [
				...platform.environments,
				{
					id: `environment-${platform.createProjectCalls.length}`,
					projectId: project.id,
					name: "Production",
					kind: "persistent",
					isProduction: true,
				},
			];
			return project;
		},
		async listEnvironments(user, projectId) {
			platform.listEnvironmentsCalls.push({ user, projectId });
			let environments = platform.environments.filter(
				(entry) => entry.projectId === projectId,
			);
			if (
				environments.length === 0 &&
				platform.projects.some((entry) => entry.id === projectId)
			) {
				const production: DashboardEnvironment = {
					id: `environment-${projectId}`,
					projectId,
					name: "Production",
					kind: "persistent",
					isProduction: true,
				};
				platform.environments.push(production);
				environments = [production];
			}
			return environments;
		},
		async getEnvironment(_, environmentId) {
			const environment = platform.environments.find(
				(entry) => entry.id === environmentId,
			);
			if (!environment) throw new Error("environment not found");
			return environment;
		},
		async createEnvironment(_, input) {
			const environment: DashboardEnvironment = {
				id: `environment-${platform.environments.length + 1}`,
				projectId: input.projectId,
				name: input.name,
				kind: "persistent",
				isProduction: false,
			};
			platform.environments.push(environment);
			return environment;
		},
		async duplicateEnvironment(_, input) {
			const source = platform.environments.find(
				(entry) => entry.id === input.sourceEnvironmentId,
			);
			if (!source) throw new Error("environment not found");
			const environment: DashboardEnvironment = {
				...source,
				id: `environment-${platform.environments.length + 1}`,
				name: input.name,
				isProduction: false,
				copiedFromEnvironmentId: source.id,
			};
			platform.environments.push(environment);
			return environment;
		},
		async renameEnvironment(_, input) {
			const environment = platform.environments.find(
				(entry) => entry.id === input.environmentId,
			);
			if (!environment) throw new Error("environment not found");
			environment.name = input.name;
			return environment;
		},
		async deleteEnvironment(_, environmentId) {
			platform.environments = platform.environments.filter(
				(entry) => entry.id !== environmentId || entry.isProduction,
			);
			platform.services = platform.services.filter(
				(entry) => entry.environmentId !== environmentId,
			);
		},
		async deployEnvironment(user, environmentId) {
			void user;
			const services = platform.services.filter(
				(entry) => entry.environmentId === environmentId,
			);
			const deployed = services.map((service) => {
				const nextDesired =
					service.spec?.desiredReplicaCount ?? service.desiredReplicaCount;
				return {
					...service,
					desiredReplicaCount: nextDesired,
					pendingChanges: false,
					unappliedChangeCount: 0,
					unappliedChanges: [],
				};
			});
			platform.services = platform.services.map((service) => {
				const next = deployed.find((entry) => entry.id === service.id);
				return next ?? service;
			});
			return deployed.map((service) => ({ service }));
		},
		async listServices(
			user,
			environmentId,
		): Promise<Array<DashboardServiceRecord>> {
			platform.listServicesCalls.push({ user, environmentId });
			if (platform.errors.listServices) {
				throw platform.errors.listServices;
			}
			return platform.services.filter(
				(service) => service.environmentId === environmentId,
			);
		},
		async waitForServices(user, input) {
			const services = await platform.listServices(user, input.environmentId);
			return {
				index: input.waitIndex + 1,
				notModified: false,
				services,
			};
		},
		async inspectRepositorySource(
			user,
			input,
		): Promise<DashboardRepositoryInspection> {
			platform.inspectRepositorySourceCalls.push({ user, ...input });
			if (platform.errors.inspectRepositorySource) {
				throw platform.errors.inspectRepositorySource;
			}
			return (
				platform.nextRepositoryInspection ?? {
					accessState: "available",
					defaultBranch: "main",
					dockerfileCandidates: ["Dockerfile"],
					recommendedBuildRecipe: {
						dockerfilePath: "Dockerfile",
						contextDir: ".",
					},
					recommendedPorts: [],
				}
			);
		},
		async linkGitHubRepository(
			user,
			input,
		): Promise<DashboardRepositoryInspection> {
			platform.linkGitHubRepositoryCalls.push({ user, ...input });
			if (platform.errors.inspectRepositorySource) {
				throw platform.errors.inspectRepositorySource;
			}
			return (
				platform.nextRepositoryInspection ?? {
					accessState: "available",
					defaultBranch: "main",
					dockerfileCandidates: ["Dockerfile"],
					recommendedBuildRecipe: {
						dockerfilePath: "Dockerfile",
						contextDir: ".",
					},
					recommendedPorts: [],
				}
			);
		},
		async createService(user, input): Promise<DashboardServiceRecord> {
			platform.createServiceCalls.push({ user, ...input });
			if (platform.errors.createService) {
				throw platform.errors.createService;
			}
			const serviceRecord: DashboardServiceRecord = {
				id: `service-${platform.services.length + 1}`,
				environmentId: input.environmentId,
				name: input.name,
				spec: input.spec,
				sourceSummary: {
					desiredSpec: input.spec.source,
					resolvedBinding: {
						repositorySelector: input.spec.source?.repositorySelector ?? "",
						trackedRef: input.spec.source?.trackedRef ?? "",
						accessState: "available",
						buildRecipe: input.spec.source?.buildRecipe,
					},
				},
				latestBuild: {
					buildId: `build-${platform.createServiceCalls.length}`,
					state: "queued",
					commitSha: "",
					imageDigest: "",
					failureReason: "",
				},
				pendingChanges: true,
			};
			platform.services = [...platform.services, serviceRecord];
			platform.serviceStatuses.set(serviceRecord.id, {
				service: serviceRecord,
				allocation: {
					allocationId: `allocation-${platform.createServiceCalls.length}`,
					serviceId: serviceRecord.id,
					agentId: "agent-1",
					desiredSpecRevision: 1,
					appliedSpecRevision: 1,
					phase: "Pending",
					message: "",
					allocationIp: "",
					healthy: false,
					updatedAt: undefined,
					desiredRolloutGeneration: 1,
					appliedRolloutGeneration: 1,
					healthyPorts: [],
				},
			});
			return serviceRecord;
		},
		async updateService(user, input): Promise<DashboardServiceRecord> {
			platform.updateServiceCalls.push({ user, ...input });
			if (platform.errors.updateService) {
				throw platform.errors.updateService;
			}
			const current = platform.services.find(
				(service) => service.id === input.serviceId,
			);
			if (!current) {
				throw new Error("service not found");
			}
			const updated: DashboardServiceRecord = {
				...current,
				name: input.name?.trim() || current.name,
				spec: input.spec,
				sourceSummary: {
					desiredSpec: input.spec.source,
					resolvedBinding: {
						repositorySelector: input.spec.source?.repositorySelector ?? "",
						trackedRef: input.spec.source?.trackedRef ?? "",
						accessState: "available",
						buildRecipe: input.spec.source?.buildRecipe,
					},
				},
			};
			platform.services = platform.services.map((service) =>
				service.id === updated.id ? updated : service,
			);
			const status = platform.serviceStatuses.get(updated.id);
			if (status) {
				platform.serviceStatuses.set(updated.id, {
					...status,
					service: updated,
				});
			}
			return updated;
		},
		async scaleService(user, input): Promise<DashboardServiceStatus> {
			platform.scaleServiceCalls.push({ user, ...input });
			if (platform.errors.scaleService) {
				throw platform.errors.scaleService;
			}
			const current = platform.services.find(
				(service) => service.id === input.serviceId,
			);
			if (!current) {
				throw new Error("service not found");
			}
			const liveDesired = current.desiredReplicaCount ?? 1;
			const replicaChange = {
				id: "desiredReplicaCount",
				section: "Replicas",
				field: "Desired count",
				path: "desiredReplicaCount",
				action:
					input.desiredReplicaCount === liveDesired
						? ("update" as const)
						: input.desiredReplicaCount > liveDesired
							? ("add" as const)
							: ("update" as const),
				currentValue: String(liveDesired),
				newValue: String(input.desiredReplicaCount),
			};
			const remainingChanges = (current.unappliedChanges ?? []).filter(
				(change) => change.id !== "desiredReplicaCount",
			);
			const unappliedChanges =
				input.desiredReplicaCount === liveDesired
					? remainingChanges
					: [...remainingChanges, replicaChange];
			const updated = {
				...current,
				spec: current.spec
					? {
							...current.spec,
							desiredReplicaCount: input.desiredReplicaCount,
						}
					: current.spec,
				pendingChanges: unappliedChanges.length > 0,
				unappliedChangeCount: unappliedChanges.length,
				unappliedChanges,
			};
			platform.services = platform.services.map((service) =>
				service.id === updated.id ? updated : service,
			);
			const status = platform.serviceStatuses.get(input.serviceId);
			if (status) {
				platform.serviceStatuses.set(input.serviceId, {
					...status,
					service: updated,
				});
			}
			return (
				platform.serviceStatuses.get(input.serviceId) ?? { service: updated }
			);
		},
		async restartService(user, input): Promise<DashboardServiceStatus> {
			platform.restartServiceCalls.push({ user, ...input });
			if (platform.errors.restartService) {
				throw platform.errors.restartService;
			}
			const current = platform.services.find(
				(service) => service.id === input.serviceId,
			);
			if (!current) {
				throw new Error("service not found");
			}
			return (
				platform.serviceStatuses.get(input.serviceId) ?? { service: current }
			);
		},
		async redeployService(user, input): Promise<DashboardServiceStatus> {
			platform.redeployServiceCalls.push({ user, ...input });
			if (platform.errors.redeployService) {
				throw platform.errors.redeployService;
			}
			const current = platform.services.find(
				(service) => service.id === input.serviceId,
			);
			if (!current) {
				throw new Error("service not found");
			}
			const updated = { ...current, pendingChanges: false };
			platform.services = platform.services.map((service) =>
				service.id === updated.id ? updated : service,
			);
			return (
				platform.serviceStatuses.get(input.serviceId) ?? { service: updated }
			);
		},
		async discardServiceChanges(user, input): Promise<DashboardServiceRecord> {
			platform.discardServiceChangesCalls.push({ user, ...input });
			if (platform.errors.discardServiceChanges) {
				throw platform.errors.discardServiceChanges;
			}
			const current = platform.services.find(
				(service) => service.id === input.serviceId,
			);
			if (!current) {
				throw new Error("service not found");
			}
			const remainingChanges = input.discardAll
				? []
				: (current.unappliedChanges ?? []).filter(
						(change) => !(input.changeIds ?? []).includes(change.id),
					);
			const updated = {
				...current,
				pendingChanges: remainingChanges.length > 0,
				unappliedChangeCount: remainingChanges.length,
				unappliedChanges: remainingChanges,
			};
			platform.services = platform.services.map((service) =>
				service.id === updated.id ? updated : service,
			);
			return updated;
		},
		async deleteService(user, input): Promise<void> {
			platform.deleteServiceCalls.push({ user, ...input });
			if (platform.errors.deleteService) {
				throw platform.errors.deleteService;
			}
			platform.services = platform.services.filter(
				(service) => service.id !== input.serviceId,
			);
			platform.serviceStatuses.delete(input.serviceId);
		},
		async getService(user, input): Promise<DashboardServiceRecord> {
			platform.getServiceCalls.push({ user, ...input });
			if (platform.errors.getService) {
				throw platform.errors.getService;
			}
			const service = platform.services.find(
				(entry) => entry.id === input.serviceId,
			);
			if (!service) {
				throw new Error("service not found");
			}
			return service;
		},
		async getServiceStatus(user, input): Promise<DashboardServiceStatus> {
			platform.getServiceStatusCalls.push({ user, ...input });
			if (platform.errors.getServiceStatus) {
				throw platform.errors.getServiceStatus;
			}
			const status = platform.serviceStatuses.get(input.serviceId);
			if (!status) {
				throw new Error("service status not found");
			}
			return status;
		},
		async waitForServiceStatus(user, input) {
			return {
				index: input.waitIndex + 1,
				notModified: false,
				status: await platform.getServiceStatus(user, input),
			};
		},
		async listServiceLogs(
			user,
			input,
		): Promise<Array<DashboardServiceLogLine>> {
			platform.listServiceLogsCalls.push({ user, ...input });
			if (platform.errors.listServiceLogs) {
				throw platform.errors.listServiceLogs;
			}
			return platform.serviceLogs.filter(
				(line) =>
					(line.allocationId === input.allocationId ||
						input.allocationId === undefined) &&
					(line.logType === input.logType || input.logType === undefined) &&
					(line.buildId === input.buildId || input.buildId === undefined) &&
					(input.search === undefined || line.line.includes(input.search)),
			);
		},
		async listServiceDeployments(
			user,
			input,
		): Promise<Array<DashboardDeploymentRecord>> {
			platform.listServiceDeploymentsCalls.push({ user, ...input });
			if (platform.errors.listServiceDeployments) {
				throw platform.errors.listServiceDeployments;
			}
			return platform.serviceDeployments;
		},
		async listDomainBindings(
			user,
			input,
		): Promise<Array<DashboardDomainBinding>> {
			platform.listDomainBindingsCalls.push({ user, ...input });
			if (platform.errors.listDomainBindings) {
				throw platform.errors.listDomainBindings;
			}
			return platform.domainBindings.filter(
				(binding) => binding.serviceId === input.serviceId,
			);
		},
		async generateDomainBinding(_, input): Promise<DashboardDomainBinding> {
			const binding: DashboardDomainBinding = {
				hostname: `violet-test.platform.example`,
				serviceId: input.serviceId,
				targetPort: input.targetPort,
				platformGenerated: true,
				ownershipState: "verified",
			};
			platform.domainBindings = [
				...platform.domainBindings.filter(
					(item) =>
						!item.platformGenerated || item.serviceId !== input.serviceId,
				),
				binding,
			];
			return binding;
		},
		async createDomainBinding(user, input): Promise<DashboardDomainBinding> {
			platform.createDomainBindingCalls.push({ user, ...input });
			if (platform.errors.createDomainBinding) {
				throw platform.errors.createDomainBinding;
			}
			const binding: DashboardDomainBinding = {
				hostname: input.hostname,
				serviceId: input.serviceId,
				targetPort: input.targetPort,
				platformGenerated: false,
				ownershipState: "unverified",
				ownershipMessage:
					"domain CNAME does not point to the service platform hostname",
			};
			platform.domainBindings = [
				...platform.domainBindings.filter(
					(item) => item.hostname !== input.hostname,
				),
				binding,
			];
			return binding;
		},
		async updateDomainBinding(_, input): Promise<DashboardDomainBinding> {
			const existing = platform.domainBindings.find(
				(b) => b.hostname === input.hostname,
			);
			if (!existing) {
				throw new Error("domain binding not found");
			}
			const updated: DashboardDomainBinding = {
				...existing,
				serviceId: input.serviceId,
				targetPort: input.targetPort,
			};
			platform.domainBindings = platform.domainBindings.map((b) =>
				b.hostname === input.hostname ? updated : b,
			);
			return updated;
		},
		async deleteDomainBinding(_, input): Promise<void> {
			platform.domainBindings = platform.domainBindings.filter(
				(b) => b.hostname !== input.hostname,
			);
		},
	};
	return platform;
}
