import type { DescMethodUnary, MessageShape } from "@bufbuild/protobuf";
import {
	Code,
	ConnectError,
	createRouterTransport,
	type HandlerContext,
} from "@connectrpc/connect";
import type {
	DashboardServiceRecord,
	DashboardUser,
	PlatformGateway,
} from "#/lib/dashboard/core/types.server";
import * as P from "#/lib/platform-gen/platform_pb";
import { createAuthenticatedPlatform } from "#/lib/platform-grpc/gateway.server";
import {
	fromPlatformJson,
	type PlatformJson,
	toPlatformJson,
} from "#/lib/platform-json";
import { jsonFixture, serviceFixture, statusFixture } from "./protocol";

type Recorded<D extends DescMethodUnary> = PlatformJson<D["input"]> & {
	user: DashboardUser;
};
type Calls = {
	[K in
		| "applyDeploymentAction"
		| "createDomainBinding"
		| "createProject"
		| "createService"
		| "deleteService"
		| "discardServiceChanges"
		| "getService"
		| "getServiceStatus"
		| "inspectSource"
		| "linkGitHubRepository"
		| "listDomainBindings"
		| "listEnvironments"
		| "listProjects"
		| "listServiceDeployments"
		| "listServiceLogs"
		| "listServices"
		| "scaleService"
		| "updateService"]: Array<Recorded<(typeof P.PlatformService.method)[K]>>;
};

export interface FakePlatformGateway extends PlatformGateway {
	calls: Calls;
	projects: PlatformJson<typeof P.ProjectSchema>[];
	environments: PlatformJson<typeof P.EnvironmentSchema>[];
	services: DashboardServiceRecord[];
	servicesIndex: number;
	serviceStatuses: Map<string, PlatformJson<typeof P.ServiceStatusSchema>>;
	serviceLogs: PlatformJson<typeof P.ServiceLogLineSchema>[];
	serviceLogGaps: PlatformJson<typeof P.ServiceLogGapSchema>[];
	volumes: PlatformJson<typeof P.VolumeSchema>[];
	buildAttempts: PlatformJson<typeof P.BuildAttemptSchema>[];
	deletionPreview: PlatformJson<typeof P.DeletionPreviewSchema>;
	serviceDeployments: PlatformJson<typeof P.DeploymentRecordSchema>[];
	domainBindings: PlatformJson<typeof P.DomainBindingSchema>[];
	nextRepositoryInspection?: PlatformJson<typeof P.InspectSourceResponseSchema>;
	fleet: PlatformJson<typeof P.FleetSchema>;
	errors: Partial<
		Record<keyof typeof P.PlatformService.method | "listFleet", Error>
	>;
}

export function createFakePlatformGateway(): FakePlatformGateway {
	const users = new Map<string, DashboardUser>();
	let gateway: PlatformGateway;
	const platform: FakePlatformGateway = {
		call(method, user, input) {
			users.set(user.id, user);
			return gateway.call(method, user, input);
		},
		calls: {
			applyDeploymentAction: [],
			createDomainBinding: [],
			createProject: [],
			createService: [],
			deleteService: [],
			discardServiceChanges: [],
			getService: [],
			getServiceStatus: [],
			inspectSource: [],
			linkGitHubRepository: [],
			listDomainBindings: [],
			listEnvironments: [],
			listProjects: [],
			listServiceDeployments: [],
			listServiceLogs: [],
			listServices: [],
			scaleService: [],
			updateService: [],
		},
		projects: [],
		environments: [],
		services: [],
		servicesIndex: 1,
		serviceStatuses: new Map(),
		serviceLogs: [],
		serviceLogGaps: [],
		volumes: [],
		buildAttempts: [],
		serviceDeployments: [],
		domainBindings: [],
		deletionPreview: jsonFixture(P.DeletionPreviewSchema, {}),
		fleet: jsonFixture(P.FleetSchema, { capacity: {} }),
		errors: {},
	};
	function user(ctx: HandlerContext): DashboardUser {
		const token = ctx.requestHeader.get("x-platform-user-assertion");
		if (!token) throw new Error("missing user assertion");
		const { sub } = JSON.parse(
			Buffer.from(token.split(".")[1], "base64url").toString(),
		);
		const result = users.get(sub);
		if (!result) throw new Error("unknown user assertion");
		return result;
	}
	function fail(operation: keyof typeof platform.errors) {
		const error = platform.errors[operation];
		if (error) throw new ConnectError(error.message, Code.Internal);
	}
	function project(id: string) {
		const result = platform.projects.find((entry) => entry.id === id);
		if (!result) throw new Error("project not found");
		return result;
	}
	function environment(id: string) {
		const result = platform.environments.find((entry) => entry.id === id);
		if (!result) throw new Error("environment not found");
		return result;
	}
	function service(id: string) {
		const result = platform.services.find((entry) => entry.id === id);
		if (!result) throw new Error("service not found");
		return result;
	}
	function nativeService(
		value: DashboardServiceRecord,
	): MessageShape<typeof P.ServiceSchema> {
		const { projectId: _, layoutPosition: __, ...resource } = value;
		return fromPlatformJson(P.ServiceSchema, resource);
	}
	function nativeStatus(id: string) {
		const result = platform.serviceStatuses.get(id);
		if (!result) throw new Error("service status not found");
		return fromPlatformJson(P.ServiceStatusSchema, result);
	}
	function replaceService(next: DashboardServiceRecord) {
		platform.services = platform.services.map((entry) =>
			entry.id === next.id ? next : entry,
		);
		const status = platform.serviceStatuses.get(next.id);
		if (status)
			platform.serviceStatuses.set(
				next.id,
				statusFixture({ ...status, service: next }),
			);
		platform.servicesIndex += 1;
	}
	function inspection() {
		return fromPlatformJson(
			P.InspectSourceResponseSchema,
			platform.nextRepositoryInspection ?? {
				accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
				defaultBranch: "main",
				dockerfileCandidates: ["Dockerfile"],
				recommendedBuildRecipe: {
					builder: "BUILDER_KIND_RAILPACK",
					contextDir: ".",
				},
				recommendedDockerfileRecipe: {
					builder: "BUILDER_KIND_DOCKERFILE",
					dockerfilePath: "Dockerfile",
					contextDir: ".",
				},
				detectedLanguage: "node",
				detectedStartCommand: "npm run start",
			},
		);
	}
	function tombstone() {
		return jsonFixture(P.DeletionStateSchema, {
			deletedAt: new Date().toISOString(),
		});
	}
	const transport = createRouterTransport((router) => {
		router.service(P.OpsService, {
			listFleet() {
				fail("listFleet");
				return fromPlatformJson(P.FleetSchema, platform.fleet);
			},
		});
		router.service(P.PlatformService, {
			listProjects(request, ctx) {
				fail("listProjects");
				platform.calls.listProjects.push({
					user: user(ctx),
					...toPlatformJson(P.ListProjectsRequestSchema, request),
				});
				return fromPlatformJson(P.ListProjectsResponseSchema, {
					projects: platform.projects.filter(
						(entry) => request.includeDeleted || !entry.deletion,
					),
				});
			},
			getProject(request) {
				return fromPlatformJson(P.ProjectSchema, project(request.projectId));
			},
			createProject(request, ctx) {
				fail("createProject");
				platform.calls.createProject.push({
					user: user(ctx),
					...toPlatformJson(P.CreateProjectRequestSchema, request),
				});
				const next = jsonFixture(P.ProjectSchema, {
					id: `project-${platform.calls.createProject.length}`,
					name: request.name,
					kind: "PROJECT_KIND_USER",
				});
				platform.projects.push(next);
				platform.environments.push(
					jsonFixture(P.EnvironmentSchema, {
						id: `environment-${platform.calls.createProject.length}`,
						projectId: next.id,
						name: "Production",
						kind: "ENVIRONMENT_KIND_PERSISTENT",
						isProduction: true,
						autoDeploy: true,
					}),
				);
				return fromPlatformJson(P.ProjectSchema, next);
			},
			updateProjectLogRetention(request) {
				const result = project(request.projectId);
				result.logRetentionDays = request.logRetentionDays;
				return fromPlatformJson(P.ProjectSchema, result);
			},
			previewProjectDeletion() {
				return fromPlatformJson(
					P.DeletionPreviewSchema,
					platform.deletionPreview,
				);
			},
			deleteProject(request) {
				const result = project(request.projectId);
				if (request.confirmationName !== result.name)
					throw new Error(
						"confirmation name does not match the current resource name",
					);
				result.deletion = tombstone();
				return {};
			},
			restoreProject(request) {
				const result = project(request.projectId);
				result.deletion = undefined;
				return fromPlatformJson(P.ProjectSchema, result);
			},
			listEnvironments(request, ctx) {
				platform.calls.listEnvironments.push({
					user: user(ctx),
					...toPlatformJson(P.ListEnvironmentsRequestSchema, request),
				});
				let entries = platform.environments.filter(
					(entry) =>
						entry.projectId === request.projectId &&
						(request.includeDeleted || !entry.deletion),
				);
				if (
					!entries.length &&
					platform.projects.some((entry) => entry.id === request.projectId)
				) {
					const next = jsonFixture(P.EnvironmentSchema, {
						id: `environment-${request.projectId}`,
						projectId: request.projectId,
						name: "Production",
						kind: "ENVIRONMENT_KIND_PERSISTENT",
						isProduction: true,
						autoDeploy: true,
					});
					platform.environments.push(next);
					entries = [next];
				}
				return fromPlatformJson(P.ListEnvironmentsResponseSchema, {
					environments: entries,
				});
			},
			getEnvironment(request) {
				return fromPlatformJson(
					P.EnvironmentSchema,
					environment(request.environmentId),
				);
			},
			createEnvironment(request) {
				const next = jsonFixture(P.EnvironmentSchema, {
					id: `environment-${platform.environments.length + 1}`,
					projectId: request.projectId,
					name: request.name,
					kind: "ENVIRONMENT_KIND_PERSISTENT",
					autoDeploy: true,
				});
				platform.environments.push(next);
				return fromPlatformJson(P.EnvironmentSchema, next);
			},
			duplicateEnvironment(request) {
				const source = environment(request.sourceEnvironmentId);
				const next = jsonFixture(P.EnvironmentSchema, {
					...source,
					id: `environment-${platform.environments.length + 1}`,
					name: request.name,
					isProduction: false,
					copiedFromEnvironmentId: source.id,
				});
				platform.environments.push(next);
				return fromPlatformJson(P.EnvironmentSchema, next);
			},
			renameEnvironment(request) {
				const result = environment(request.environmentId);
				result.name = request.name;
				return fromPlatformJson(P.EnvironmentSchema, result);
			},
			updateEnvironmentAutoDeploy(request) {
				const result = environment(request.environmentId);
				result.autoDeploy = request.autoDeploy;
				return fromPlatformJson(P.EnvironmentSchema, result);
			},
			previewEnvironmentDeletion() {
				return fromPlatformJson(
					P.DeletionPreviewSchema,
					platform.deletionPreview,
				);
			},
			deleteEnvironment(request) {
				const result = environment(request.environmentId);
				if (result.isProduction && request.confirmationName !== result.name)
					throw new Error(
						"confirmation name does not match the current resource name",
					);
				result.deletion = tombstone();
				platform.services = platform.services.filter(
					(entry) => entry.environmentId !== result.id,
				);
				return {};
			},
			restoreEnvironment(request) {
				const result = environment(request.environmentId);
				result.deletion = undefined;
				return fromPlatformJson(P.EnvironmentSchema, result);
			},
			releaseEnvironment(request) {
				const services = platform.services
					.filter((entry) => entry.environmentId === request.environmentId)
					.map((entry) => {
						const next = {
							...entry,
							desiredReplicaCount:
								entry.spec?.desiredReplicaCount ?? entry.desiredReplicaCount,
							pendingChanges: false,
							unappliedChangeCount: 0,
							unappliedChanges: [],
						};
						replaceService(next);
						return { service: nativeService(next) };
					});
				return { services };
			},
			listServices(request, ctx) {
				fail("listServices");
				platform.calls.listServices.push({
					user: user(ctx),
					...toPlatformJson(P.ListServicesRequestSchema, request),
				});
				return {
					index:
						BigInt(platform.servicesIndex) > request.waitIndex
							? BigInt(platform.servicesIndex)
							: request.waitIndex + 1n,
					notModified: false,
					services: platform.services
						.filter(
							(entry) =>
								entry.environmentId === request.environmentId &&
								(request.includeDeleted || !entry.deletion),
						)
						.map(nativeService),
				};
			},
			inspectSource(request, ctx) {
				fail("inspectSource");
				platform.calls.inspectSource.push({
					user: user(ctx),
					...toPlatformJson(P.InspectSourceRequestSchema, request),
				});
				return inspection();
			},
			linkGitHubRepository(request, ctx) {
				fail("inspectSource");
				platform.calls.linkGitHubRepository.push({
					user: user(ctx),
					...toPlatformJson(P.LinkGitHubRepositoryRequestSchema, request),
				});
				return inspection();
			},
			createService(request, ctx) {
				fail("createService");
				platform.calls.createService.push({
					user: user(ctx),
					...toPlatformJson(P.CreateServiceRequestSchema, request),
				});
				if (!request.service?.spec) throw new Error("service spec missing");
				const spec = toPlatformJson(P.ServiceSpecSchema, request.service.spec);
				const next = serviceFixture({
					id: `service-${platform.services.length + 1}`,
					environmentId: request.environmentId,
					name: request.service.name,
					spec,
					sourceSummary: {
						sourceState: {
							desiredSpec: spec.source?.sourceSpec,
							resolvedBinding: {
								...spec.source?.sourceSpec,
								accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
							},
						},
					},
					latestBuild: {
						buildId: `build-${platform.calls.createService.length}`,
						state: "BUILD_STATE_QUEUED",
					},
					pendingChanges: true,
				});
				platform.services.push(next);
				platform.servicesIndex += 1;
				platform.serviceStatuses.set(
					next.id,
					statusFixture({
						service: next,
						allocation: {
							allocationId: `allocation-${platform.calls.createService.length}`,
							serviceId: next.id,
							agentId: "agent-1",
							desiredSpecRevision: "1",
							appliedSpecRevision: "1",
							phase: "Pending",
							desiredRolloutGeneration: "1",
							appliedRolloutGeneration: "1",
						},
					}),
				);
				return nativeService(next);
			},
			updateService(request, ctx) {
				fail("updateService");
				platform.calls.updateService.push({
					user: user(ctx),
					...toPlatformJson(P.UpdateServiceRequestSchema, request),
				});
				if (!request.service?.spec) throw new Error("service spec missing");
				const current = service(request.serviceId);
				const spec = toPlatformJson(P.ServiceSpecSchema, request.service.spec);
				const next = {
					...current,
					name: request.service.name?.trim() || current.name,
					spec,
					sourceSummary: jsonFixture(P.ServiceSourceSummarySchema, {
						sourceState: {
							desiredSpec: spec.source?.sourceSpec,
							resolvedBinding: {
								...spec.source?.sourceSpec,
								accessState: "SOURCE_ACCESS_STATE_AVAILABLE",
							},
						},
					}),
				};
				replaceService(next);
				return nativeService(next);
			},
			scaleService(request, ctx) {
				fail("scaleService");
				platform.calls.scaleService.push({
					user: user(ctx),
					...toPlatformJson(P.ScaleServiceRequestSchema, request),
				});
				const current = service(request.serviceId);
				const live = current.desiredReplicaCount || 1;
				const changes = current.unappliedChanges.filter(
					(entry) => entry.id !== "desiredReplicaCount",
				);
				if (request.desiredReplicaCount !== live)
					changes.push(
						jsonFixture(P.ServiceUnappliedChangeSchema, {
							id: "desiredReplicaCount",
							section: "Replicas",
							field: "Desired count",
							path: "desiredReplicaCount",
							action:
								request.desiredReplicaCount > live
									? "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD"
									: "SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE",
							currentValue: String(live),
							newValue: String(request.desiredReplicaCount),
						}),
					);
				const next = {
					...current,
					spec: current.spec
						? {
								...current.spec,
								desiredReplicaCount: request.desiredReplicaCount,
							}
						: undefined,
					pendingChanges: changes.length > 0,
					unappliedChangeCount: changes.length,
					unappliedChanges: changes,
				};
				replaceService(next);
				const status = platform.serviceStatuses.get(next.id);
				return status
					? fromPlatformJson(P.ServiceStatusSchema, status)
					: { service: nativeService(next) };
			},
			applyDeploymentAction(request, ctx) {
				fail("applyDeploymentAction");
				platform.calls.applyDeploymentAction.push({
					user: user(ctx),
					...toPlatformJson(P.ApplyDeploymentActionRequestSchema, request),
				});
				const status = platform.serviceStatuses.get(request.serviceId);
				return status
					? fromPlatformJson(P.ServiceStatusSchema, status)
					: { service: nativeService(service(request.serviceId)) };
			},
			discardServiceChanges(request, ctx) {
				fail("discardServiceChanges");
				platform.calls.discardServiceChanges.push({
					user: user(ctx),
					...toPlatformJson(P.DiscardServiceChangesRequestSchema, request),
				});
				const current = service(request.serviceId);
				const changes = request.discardAll
					? []
					: current.unappliedChanges.filter(
							(entry) => !request.changeIds.includes(entry.id),
						);
				const next = {
					...current,
					pendingChanges: changes.length > 0,
					unappliedChangeCount: changes.length,
					unappliedChanges: changes,
				};
				replaceService(next);
				return nativeService(next);
			},
			deleteService(request, ctx) {
				fail("deleteService");
				platform.calls.deleteService.push({
					user: user(ctx),
					...toPlatformJson(P.DeleteServiceRequestSchema, request),
				});
				platform.services = platform.services.filter(
					(entry) => entry.id !== request.serviceId,
				);
				platform.servicesIndex += 1;
				platform.serviceStatuses.delete(request.serviceId);
				return {};
			},
			restoreService(request) {
				const result = service(request.serviceId);
				result.deletion = undefined;
				return nativeService(result);
			},
			listVolumes(request) {
				return fromPlatformJson(P.ListVolumesResponseSchema, {
					volumes: platform.volumes.filter(
						(entry) =>
							entry.environmentId === request.environmentId &&
							(request.includeDeleted || !entry.deletion),
					),
				});
			},
			previewVolumeDeletion() {
				return fromPlatformJson(
					P.DeletionPreviewSchema,
					platform.deletionPreview,
				);
			},
			deleteVolume(request) {
				const result = platform.volumes.find(
					(entry) => entry.id === request.volumeId,
				);
				if (!result) throw new Error("volume not found");
				if (request.confirmationName !== result.name)
					throw new Error(
						"confirmation name does not match the current resource name",
					);
				result.deletion = tombstone();
				return {};
			},
			listBuildAttempts() {
				return fromPlatformJson(P.ListBuildAttemptsResponseSchema, {
					attempts: platform.buildAttempts,
				});
			},
			getService(request, ctx) {
				fail("getService");
				platform.calls.getService.push({
					user: user(ctx),
					...toPlatformJson(P.GetServiceRequestSchema, request),
				});
				return nativeService(service(request.serviceId));
			},
			getServiceStatus(request, ctx) {
				fail("getServiceStatus");
				platform.calls.getServiceStatus.push({
					user: user(ctx),
					...toPlatformJson(P.GetServiceStatusRequestSchema, request),
				});
				return {
					...nativeStatus(request.serviceId),
					index: request.waitIndex + 1n,
					notModified: false,
				};
			},
			listServiceLogs(request, ctx) {
				fail("listServiceLogs");
				platform.calls.listServiceLogs.push({
					user: user(ctx),
					...toPlatformJson(P.ListServiceLogsRequestSchema, request),
				});
				return fromPlatformJson(P.ListServiceLogsResponseSchema, {
					lines: platform.serviceLogs.filter(
						(entry) =>
							(!request.allocationId ||
								entry.allocationId === request.allocationId) &&
							(!request.logType ||
								entry.logType ===
									toPlatformJson(P.ListServiceLogsRequestSchema, request)
										.logType) &&
							(!request.buildId || entry.buildId === request.buildId) &&
							(!request.search || entry.line.includes(request.search)),
					),
					gaps: platform.serviceLogGaps,
				});
			},
			listServiceDeployments(request, ctx) {
				fail("listServiceDeployments");
				platform.calls.listServiceDeployments.push({
					user: user(ctx),
					...toPlatformJson(P.ListServiceDeploymentsRequestSchema, request),
				});
				return fromPlatformJson(P.ListServiceDeploymentsResponseSchema, {
					deployments: platform.serviceDeployments,
				});
			},
			listDomainBindings(request, ctx) {
				fail("listDomainBindings");
				platform.calls.listDomainBindings.push({
					user: user(ctx),
					...toPlatformJson(P.ListDomainBindingsRequestSchema, request),
				});
				return fromPlatformJson(P.ListDomainBindingsResponseSchema, {
					bindings: platform.domainBindings.filter(
						(entry) =>
							entry.serviceId === request.serviceId &&
							(request.includeDeleted || !entry.deletion),
					),
				});
			},
			generateDomainBinding(request) {
				const binding = jsonFixture(P.DomainBindingSchema, {
					hostname: "violet-test.platform.example",
					serviceId: request.serviceId,
					targetPort: request.targetPort,
					platformGenerated: true,
					ownershipState: "DOMAIN_OWNERSHIP_STATE_VERIFIED",
				});
				platform.domainBindings = [
					...platform.domainBindings.filter(
						(entry) =>
							!entry.platformGenerated || entry.serviceId !== request.serviceId,
					),
					binding,
				];
				return fromPlatformJson(P.DomainBindingSchema, binding);
			},
			createDomainBinding(request, ctx) {
				fail("createDomainBinding");
				platform.calls.createDomainBinding.push({
					user: user(ctx),
					...toPlatformJson(P.CreateDomainBindingRequestSchema, request),
				});
				if (!request.binding) throw new Error("binding missing");
				const binding = jsonFixture(P.DomainBindingSchema, {
					...toPlatformJson(P.DomainBindingInputSchema, request.binding),
					ownershipState: "DOMAIN_OWNERSHIP_STATE_UNVERIFIED",
					ownershipMessage:
						"domain CNAME does not point to the service platform hostname",
				});
				platform.domainBindings = [
					...platform.domainBindings.filter(
						(entry) => entry.hostname !== binding.hostname,
					),
					binding,
				];
				return fromPlatformJson(P.DomainBindingSchema, binding);
			},
			updateDomainBinding(request) {
				const binding = platform.domainBindings.find(
					(entry) => entry.hostname === request.hostname,
				);
				if (!binding || !request.binding)
					throw new Error("domain binding not found");
				Object.assign(binding, {
					serviceId: request.binding.serviceId,
					targetPort: request.binding.targetPort,
				});
				return fromPlatformJson(P.DomainBindingSchema, binding);
			},
			deleteDomainBinding(request) {
				platform.domainBindings = platform.domainBindings.filter(
					(entry) => entry.hostname !== request.hostname,
				);
				return {};
			},
			restoreDomainBinding(request) {
				const binding = platform.domainBindings.find(
					(entry) => entry.hostname === request.hostname,
				);
				if (!binding) throw new Error("domain binding not found");
				binding.deletion = undefined;
				return fromPlatformJson(P.DomainBindingSchema, binding);
			},
		});
	});
	gateway = createAuthenticatedPlatform(
		transport,
		"test-platform-assertion-secret-32-bytes-minimum",
	);
	return platform;
}
