import type {
	DashboardBuildAttempt,
	DashboardDeletionPreview,
	DashboardDeploymentAction,
	DashboardDeploymentRecord,
	DashboardDomainBinding,
	DashboardEnvironment,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardServiceLogGap,
	DashboardServiceLogLine,
	DashboardServiceLogType,
	DashboardServiceRecord,
	DashboardServiceSecret,
	DashboardServiceSpec,
	DashboardServiceStatus,
	DashboardUser,
	DashboardVolume,
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
	applyDeploymentActionCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		deploymentId: string;
		action: DashboardDeploymentAction;
		idempotencyKey: string;
		allocationId?: string;
	}>;
	scaleServiceCalls: Array<{
		user: DashboardUser;
		serviceId: string;
		desiredReplicaCount: number;
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
		pageToken?: string;
		gapPageToken?: string;
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
	servicesIndex: number;
	serviceStatuses: Map<string, DashboardServiceStatus>;
	serviceLogs: Array<DashboardServiceLogLine>;
	serviceLogGaps: Array<DashboardServiceLogGap>;
	volumes: Array<DashboardVolume>;
	serviceSecrets: Map<string, Array<DashboardServiceSecret>>;
	buildAttempts: Array<DashboardBuildAttempt>;
	deletionPreview: DashboardDeletionPreview;
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
		applyDeploymentAction?: Error;
		scaleService?: Error;
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
		applyDeploymentActionCalls: [],
		scaleServiceCalls: [],
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
		servicesIndex: 1,
		serviceStatuses: new Map<string, DashboardServiceStatus>(),
		serviceLogs: [],
		serviceLogGaps: [],
		volumes: [],
		serviceSecrets: new Map<string, Array<DashboardServiceSecret>>(),
		buildAttempts: [],
		deletionPreview: {
			environments: [],
			services: [],
			domains: [],
			volumes: [],
		},
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
		async listProjects(user, options): Promise<Array<DashboardProject>> {
			platform.listProjectsCalls.push(user);
			if (platform.errors.listProjects) {
				throw platform.errors.listProjects;
			}
			return platform.projects.filter(
				(project) => options?.includeDeleted || !project.deletion,
			);
		},
		async getProject(_, projectId) {
			const project = platform.projects.find((entry) => entry.id === projectId);
			if (!project) throw new Error("project not found");
			return project;
		},
		async updateProjectLogRetention(_, input) {
			const project = await platform.getProject(_, input.projectId);
			project.logRetentionDays = input.logRetentionDays;
			return project;
		},
		async previewProjectDeletion() {
			return platform.deletionPreview;
		},
		async deleteProject(_, input) {
			const project = await platform.getProject(_, input.projectId);
			if (input.confirmationName !== project.name) {
				throw new Error(
					"confirmation name does not match the current resource name",
				);
			}
			project.deletion = { deletedAt: new Date(), inherited: false };
		},
		async restoreProject(_, projectId) {
			const project = await platform.getProject(_, projectId);
			project.deletion = undefined;
			return project;
		},
		async createProject(user, name): Promise<DashboardProject> {
			platform.createProjectCalls.push({ user, name });
			if (platform.errors.createProject) {
				throw platform.errors.createProject;
			}
			const project: DashboardProject = {
				id: `project-${platform.createProjectCalls.length}`,
				name,
				kind: "PROJECT_KIND_USER",
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
					autoDeploy: true,
				},
			];
			return project;
		},
		async listEnvironments(user, projectId, options) {
			platform.listEnvironmentsCalls.push({ user, projectId });
			let environments = platform.environments.filter(
				(entry) =>
					entry.projectId === projectId &&
					(options?.includeDeleted || !entry.deletion),
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
					autoDeploy: true,
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
				autoDeploy: true,
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
		async updateEnvironmentAutoDeploy(_, input) {
			const environment = platform.environments.find(
				(entry) => entry.id === input.environmentId,
			);
			if (!environment) throw new Error("environment not found");
			environment.autoDeploy = input.autoDeploy;
			return environment;
		},
		async previewEnvironmentDeletion() {
			return platform.deletionPreview;
		},
		async deleteEnvironment(_, input) {
			const environment = await platform.getEnvironment(_, input.environmentId);
			if (
				environment.isProduction &&
				input.confirmationName !== environment.name
			) {
				throw new Error(
					"confirmation name does not match the current resource name",
				);
			}
			environment.deletion = { deletedAt: new Date(), inherited: false };
			platform.services = platform.services.filter(
				(entry) => entry.environmentId !== input.environmentId,
			);
		},
		async restoreEnvironment(_, environmentId) {
			const environment = await platform.getEnvironment(_, environmentId);
			environment.deletion = undefined;
			return environment;
		},
		async releaseEnvironment(user, environmentId) {
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
			platform.servicesIndex += 1;
			return deployed.map((service) => ({ service }));
		},
		async listServices(user, environmentId, options) {
			platform.listServicesCalls.push({ user, environmentId });
			if (platform.errors.listServices) {
				throw platform.errors.listServices;
			}
			return {
				index: platform.servicesIndex,
				notModified: false,
				services: platform.services.filter(
					(service) =>
						service.environmentId === environmentId &&
						(options?.includeDeleted || !service.deletion),
				),
			};
		},
		async waitForServices(user, input) {
			const snapshot = await platform.listServices(user, input.environmentId);
			return {
				index: Math.max(snapshot.index, input.waitIndex + 1),
				notModified: false,
				services: snapshot.services,
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
					accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
					defaultBranch: "main",
					dockerfileCandidates: ["Dockerfile"],
					recommendedBuildRecipe: {
						builder: "BUILDER_KIND_RAILPACK",
						dockerfilePath: "",
						contextDir: ".",
					},
					recommendedDockerfileRecipe: {
						builder: "BUILDER_KIND_DOCKERFILE",
						dockerfilePath: "Dockerfile",
						contextDir: ".",
					},
					recommendedPorts: [],
					detectedLanguage: "node",
					detectedStartCommand: "npm run start",
					analysisError: "",
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
					accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
					defaultBranch: "main",
					dockerfileCandidates: ["Dockerfile"],
					recommendedBuildRecipe: {
						builder: "BUILDER_KIND_RAILPACK",
						dockerfilePath: "",
						contextDir: ".",
					},
					recommendedDockerfileRecipe: {
						builder: "BUILDER_KIND_DOCKERFILE",
						dockerfilePath: "Dockerfile",
						contextDir: ".",
					},
					recommendedPorts: [],
					detectedLanguage: "node",
					detectedStartCommand: "npm run start",
					analysisError: "",
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
						accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
						buildRecipe: input.spec.source?.buildRecipe,
					},
				},
				latestBuild: {
					buildId: `build-${platform.createServiceCalls.length}`,
					state: "BUILD_STATE_QUEUED",
					commitSha: "",
					imageDigest: "",
					failureReason: "",
				},
				pendingChanges: true,
			};
			platform.services = [...platform.services, serviceRecord];
			platform.servicesIndex += 1;
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
					allocationIpv4: "",
					allocationIpv6: "",
					healthy: false,
					updatedAt: undefined,
					desiredRolloutGeneration: 1,
					appliedRolloutGeneration: 1,
					healthyIpv4Ports: [],
					healthyIpv6Ports: [],
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
						accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
						buildRecipe: input.spec.source?.buildRecipe,
					},
				},
			};
			platform.services = platform.services.map((service) =>
				service.id === updated.id ? updated : service,
			);
			platform.servicesIndex += 1;
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
						? ("SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE" as const)
						: input.desiredReplicaCount > liveDesired
							? ("SERVICE_UNAPPLIED_CHANGE_ACTION_ADD" as const)
							: ("SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE" as const),
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
			platform.servicesIndex += 1;
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
		async applyDeploymentAction(user, input): Promise<DashboardServiceStatus> {
			platform.applyDeploymentActionCalls.push({ user, ...input });
			if (platform.errors.applyDeploymentAction) {
				throw platform.errors.applyDeploymentAction;
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
			platform.servicesIndex += 1;
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
			platform.servicesIndex += 1;
			platform.serviceStatuses.delete(input.serviceId);
		},
		async restoreService(_, serviceId): Promise<DashboardServiceRecord> {
			const service = platform.services.find((entry) => entry.id === serviceId);
			if (!service) throw new Error("service not found");
			service.deletion = undefined;
			return service;
		},
		async listServiceSecrets(_, serviceId) {
			return platform.serviceSecrets.get(serviceId) ?? [];
		},
		async sealServiceSecret(_, input) {
			const current = platform.serviceSecrets.get(input.serviceId) ?? [];
			const previous = current.find((entry) => entry.name === input.name);
			const sealed: DashboardServiceSecret = {
				name: input.name,
				version: (previous?.version ?? 0) + 1,
				updatedAt: new Date(),
			};
			platform.serviceSecrets.set(input.serviceId, [
				...current.filter((entry) => entry.name !== input.name),
				sealed,
			]);
			return sealed;
		},
		async deleteServiceSecret(_, input) {
			platform.serviceSecrets.set(
				input.serviceId,
				(platform.serviceSecrets.get(input.serviceId) ?? []).filter(
					(entry) => entry.name !== input.name,
				),
			);
		},
		async listVolumes(_, environmentId, options) {
			return platform.volumes.filter(
				(volume) =>
					volume.environmentId === environmentId &&
					(options?.includeDeleted || !volume.deletion),
			);
		},
		async previewVolumeDeletion() {
			return platform.deletionPreview;
		},
		async deleteVolume(_, input) {
			const volume = platform.volumes.find(
				(entry) => entry.id === input.volumeId,
			);
			if (!volume) throw new Error("volume not found");
			if (input.confirmationName !== volume.name) {
				throw new Error(
					"confirmation name does not match the current resource name",
				);
			}
			volume.deletion = { deletedAt: new Date(), inherited: false };
		},
		async listBuildAttempts() {
			return platform.buildAttempts;
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
		async listServiceLogs(user, input) {
			platform.listServiceLogsCalls.push({ user, ...input });
			if (platform.errors.listServiceLogs) {
				throw platform.errors.listServiceLogs;
			}
			return {
				lines: platform.serviceLogs.filter(
					(line) =>
						(line.allocationId === input.allocationId ||
							input.allocationId === undefined) &&
						(line.logType === input.logType || input.logType === undefined) &&
						(line.buildId === input.buildId || input.buildId === undefined) &&
						(input.search === undefined || line.line.includes(input.search)),
				),
				gaps: platform.serviceLogGaps,
			};
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
				ownershipState: "DOMAIN_OWNERSHIP_STATE_VERIFIED",
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
				ownershipState: "DOMAIN_OWNERSHIP_STATE_UNVERIFIED",
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
		async restoreDomainBinding(_, hostname): Promise<DashboardDomainBinding> {
			const binding = platform.domainBindings.find(
				(entry) => entry.hostname === hostname,
			);
			if (!binding) throw new Error("domain binding not found");
			binding.deletion = undefined;
			return binding;
		},
	};
	return platform;
}
