import type {
	DashboardDomainBinding,
	DashboardProject,
	DashboardRepositoryInspection,
	DashboardServiceRecord,
	DashboardServiceSpec,
	DashboardServiceStatus,
	DashboardUser,
	PlatformGateway,
} from "#/lib/dashboard/core/types.server";

export interface FakePlatformGateway extends PlatformGateway {
	ensurePrincipalCalls: Array<DashboardUser>;
	listProjectsCalls: Array<DashboardUser>;
	createProjectCalls: Array<{ user: DashboardUser; name: string }>;
	listServicesCalls: Array<{ user: DashboardUser; projectId: string }>;
	inspectRepositorySourceCalls: Array<{
		user: DashboardUser;
		provider: string;
		repositorySelector: string;
	}>;
	createServiceCalls: Array<{
		user: DashboardUser;
		projectId: string;
		name: string;
		spec: DashboardServiceSpec;
	}>;
	updateServiceCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
		name?: string;
		spec: DashboardServiceSpec;
	}>;
	getServiceCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
	}>;
	getServiceStatusCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
	}>;
	listDomainBindingsCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
	}>;
	createDomainBindingCalls: Array<{
		user: DashboardUser;
		projectId: string;
		serviceId: string;
		hostname: string;
		targetPort: number;
	}>;
	projects: Array<DashboardProject>;
	services: Array<DashboardServiceRecord>;
	serviceStatuses: Map<string, DashboardServiceStatus>;
	domainBindings: Array<DashboardDomainBinding>;
	nextRepositoryInspection?: DashboardRepositoryInspection;
	errors: {
		ensurePrincipal?: Error;
		listProjects?: Error;
		createProject?: Error;
		listServices?: Error;
		inspectRepositorySource?: Error;
		createService?: Error;
		updateService?: Error;
		getService?: Error;
		getServiceStatus?: Error;
		listDomainBindings?: Error;
		createDomainBinding?: Error;
	};
}

export function createFakePlatformGateway(): FakePlatformGateway {
	const platform: FakePlatformGateway = {
		ensurePrincipalCalls: [],
		listProjectsCalls: [],
		createProjectCalls: [],
		listServicesCalls: [],
		inspectRepositorySourceCalls: [],
		createServiceCalls: [],
		updateServiceCalls: [],
		getServiceCalls: [],
		getServiceStatusCalls: [],
		listDomainBindingsCalls: [],
		createDomainBindingCalls: [],
		projects: [],
		services: [],
		serviceStatuses: new Map<string, DashboardServiceStatus>(),
		domainBindings: [],
		nextRepositoryInspection: undefined,
		errors: {},
		async ensurePrincipal(user): Promise<void> {
			platform.ensurePrincipalCalls.push(user);
			if (platform.errors.ensurePrincipal) {
				throw platform.errors.ensurePrincipal;
			}
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
			return project;
		},
		async listServices(
			user,
			projectId,
		): Promise<Array<DashboardServiceRecord>> {
			platform.listServicesCalls.push({ user, projectId });
			if (platform.errors.listServices) {
				throw platform.errors.listServices;
			}
			return platform.services.filter(
				(service) => service.projectId === projectId,
			);
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
		async createService(user, input): Promise<DashboardServiceRecord> {
			platform.createServiceCalls.push({ user, ...input });
			if (platform.errors.createService) {
				throw platform.errors.createService;
			}
			const serviceRecord: DashboardServiceRecord = {
				id: `service-${platform.services.length + 1}`,
				projectId: input.projectId,
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
			};
			platform.services = [...platform.services, serviceRecord];
			platform.serviceStatuses.set(serviceRecord.id, {
				service: serviceRecord,
				allocation: {
					phase: "Pending",
					message: "",
					allocationIp: "",
					healthy: false,
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
				(service) =>
					service.projectId === input.projectId &&
					service.id === input.serviceId,
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
		async getService(user, input): Promise<DashboardServiceRecord> {
			platform.getServiceCalls.push({ user, ...input });
			if (platform.errors.getService) {
				throw platform.errors.getService;
			}
			const service = platform.services.find(
				(entry) =>
					entry.projectId === input.projectId && entry.id === input.serviceId,
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
		async listDomainBindings(
			user,
			input,
		): Promise<Array<DashboardDomainBinding>> {
			platform.listDomainBindingsCalls.push({ user, ...input });
			if (platform.errors.listDomainBindings) {
				throw platform.errors.listDomainBindings;
			}
			return platform.domainBindings.filter(
				(binding) =>
					binding.projectId === input.projectId &&
					binding.serviceId === input.serviceId,
			);
		},
		async createDomainBinding(user, input): Promise<DashboardDomainBinding> {
			platform.createDomainBindingCalls.push({ user, ...input });
			if (platform.errors.createDomainBinding) {
				throw platform.errors.createDomainBinding;
			}
			const binding: DashboardDomainBinding = {
				hostname: input.hostname,
				projectId: input.projectId,
				serviceId: input.serviceId,
				targetPort: input.targetPort,
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
				(b) => b.hostname === input.hostname && b.projectId === input.projectId,
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
				(b) =>
					!(b.hostname === input.hostname && b.projectId === input.projectId),
			);
		},
	};
	return platform;
}
