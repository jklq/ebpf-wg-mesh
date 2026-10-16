import type * as Protocol from "#/lib/platform-gen/platform_pb";
import type { PlatformJson } from "#/lib/platform-json";
export type DashboardProject = PlatformJson<typeof Protocol.ProjectSchema>;
export type DashboardDeletionState = PlatformJson<
	typeof Protocol.DeletionStateSchema
>;
export type DashboardDeletionPreview = PlatformJson<
	typeof Protocol.DeletionPreviewSchema
>;
export type DashboardVolume = PlatformJson<typeof Protocol.VolumeSchema>;
export type DashboardEnvironment = PlatformJson<
	typeof Protocol.EnvironmentSchema
>;
export type DashboardDeploymentActionRecord = PlatformJson<
	typeof Protocol.DeploymentActionRecordSchema
>;
export type DashboardDeploymentStatus = PlatformJson<
	typeof Protocol.DeploymentStatusSchema
>;
export type DashboardBuildRecipe = PlatformJson<
	typeof Protocol.BuildRecipeSchema
>;
export type DashboardRepositoryInspection = PlatformJson<
	typeof Protocol.InspectSourceResponseSchema
>;
export type DashboardSourceSpec = PlatformJson<
	typeof Protocol.ServiceSourceSpecSchema
>;
export type DashboardRuntimePort = PlatformJson<
	typeof Protocol.ServiceRuntimePortSchema
>;
export type DashboardHTTPHealthCheck = PlatformJson<
	typeof Protocol.HealthCheckSchema
>;
export type DashboardRestartSpec = PlatformJson<
	typeof Protocol.ServiceRestartSchema
>;
export type DashboardRuntimeSpec = PlatformJson<
	typeof Protocol.ServiceRuntimeSchema
>;
export type DashboardServiceSpec = PlatformJson<
	typeof Protocol.ServiceSpecSchema
>;
export type DashboardRollingStrategy = PlatformJson<
	typeof Protocol.RollingStrategySchema
>;
export type DashboardFleetAgent = PlatformJson<typeof Protocol.AgentSchema>;
export type DashboardFleet = PlatformJson<typeof Protocol.FleetSchema>;
export type DashboardAgentEnrollment = PlatformJson<
	typeof Protocol.AgentEnrollmentSchema
>;
export type DashboardResolvedSourceBinding = PlatformJson<
	typeof Protocol.ResolvedSourceBindingSchema
>;
export type DashboardBuildArtifact = PlatformJson<
	typeof Protocol.BuildArtifactSchema
>;
export type DashboardBuildStatus = PlatformJson<
	typeof Protocol.BuildStatusSchema
>;
export type DashboardBuildAttempt = PlatformJson<
	typeof Protocol.BuildAttemptSchema
>;
export type DashboardDeploymentStage = PlatformJson<
	typeof Protocol.DeploymentStageSchema
>;
export type DashboardSourceRevision = PlatformJson<
	typeof Protocol.SourceRevisionSchema
>;
export type DashboardServiceSourceSummary = PlatformJson<
	typeof Protocol.ServiceSourceSummarySchema
>;
export type DashboardUnappliedChange = PlatformJson<
	typeof Protocol.ServiceUnappliedChangeSchema
>;
export type DashboardRestartObservation = PlatformJson<
	typeof Protocol.RestartObservationSchema
>;
export type DashboardAllocationStatus = PlatformJson<
	typeof Protocol.AllocationStatusSchema
>;
export type DashboardServiceStatus = PlatformJson<
	typeof Protocol.ServiceStatusSchema
>;
export type DashboardServiceLogLine = PlatformJson<
	typeof Protocol.ServiceLogLineSchema
>;
export type DashboardServiceLogGap = PlatformJson<
	typeof Protocol.ServiceLogGapSchema
>;
export type DashboardServiceLogPage = PlatformJson<
	typeof Protocol.ListServiceLogsResponseSchema
>;
export type DashboardDeploymentRecord = PlatformJson<
	typeof Protocol.DeploymentRecordSchema
>;
export type DashboardProjectUsage = PlatformJson<
	typeof Protocol.ProjectUsageSchema
>;
export type DashboardUsageQuantities = PlatformJson<
	typeof Protocol.UsageQuantitiesSchema
>;
export type DashboardUsageBucket = PlatformJson<
	typeof Protocol.UsageBucketSchema
>;
export type DashboardUsageResource = PlatformJson<
	typeof Protocol.UsageResourceSchema
>;
export type DashboardUsageGap = PlatformJson<typeof Protocol.UsageGapSchema>;
export type DashboardUsageResolution = Exclude<
	Protocol.UsageResolutionJson,
	"USAGE_RESOLUTION_UNSPECIFIED"
>;
export type DashboardBillingSummary = PlatformJson<
	typeof Protocol.BillingAccountSummarySchema
>;
export type DashboardBillingRestriction = PlatformJson<
	typeof Protocol.BillingRestrictionStateSchema
>;
export type DashboardBillingProjectCost = PlatformJson<
	typeof Protocol.BillingProjectCostSchema
>;
export type DashboardBillingRate = PlatformJson<
	typeof Protocol.BillingRateSchema
>;
export type DashboardBillingStatement = PlatformJson<
	typeof Protocol.BillingStatementSchema
>;
export type DashboardBillingStatementLine = PlatformJson<
	typeof Protocol.BillingStatementLineSchema
>;
/** Billing reads fail with FailedPrecondition when the installation has billing off. */
export type DashboardBillingAvailability =
	| { available: true; summary: DashboardBillingSummary }
	| { available: false; reason: string };
export type DashboardProjectKind = Protocol.ProjectKindJson;
export type RepositoryAccessState = Protocol.SourceAccessStateJson;
export type DashboardBuildState = Protocol.BuildStateJson;
export type DashboardDeploymentAction = Protocol.DeploymentActionJson;
export type DashboardDeploymentStageState = Protocol.DeploymentStageStateJson;
export type DashboardDeploymentState = Protocol.DeploymentStateJson;
export type DashboardDeploymentCauseKind = Protocol.DeploymentCauseKindJson;
export type DashboardServiceLogType = Protocol.ServiceLogTypeJson;
export type DashboardBuilderKind = Protocol.BuilderKindJson;
export type DashboardRestartPolicy = Protocol.RestartPolicyJson;
export type DashboardAgentLifecycleState = Protocol.AgentLifecycleStateJson;
export type DashboardUnappliedChangeAction =
	Protocol.ServiceUnappliedChangeActionJson;
export type DashboardDomainOwnershipState = Protocol.DomainOwnershipStateJson;
export type DashboardServiceRecord = PlatformJson<
	typeof Protocol.ServiceSchema
> & {
	projectId?: string;
	layoutPosition?: DashboardServicePosition;
};
export type DashboardDomainBinding = PlatformJson<
	typeof Protocol.DomainBindingSchema
> & { projectId?: string };
export type FleetAgentInput = Omit<
	PlatformJson<typeof Protocol.CreateAgentRequestSchema>,
	"reservedCpuMillis" | "reservedMemoryMebibytes"
> & {
	reservedCpuMillis: number;
	reservedMemoryMebibytes: number;
};
export interface DashboardUser {
	id: string;
	email: string;
}

export interface DashboardUserProfile extends DashboardUser {
	githubLogin?: string;
}

export type DashboardWorkspace = PlatformJson<typeof Protocol.WorkspaceSchema>;
export type DashboardWorkspaceInvitation = PlatformJson<
	typeof Protocol.WorkspaceInvitationSchema
>;
export type DashboardWorkspaceRole = Exclude<
	Protocol.WorkspaceRoleJson,
	"WORKSPACE_ROLE_UNSPECIFIED"
>;

/** The signed-in user's workspaces, with the one being viewed. */
export interface DashboardWorkspaceShell {
	user: DashboardUser;
	githubLogin?: string;
	workspace: DashboardWorkspace;
	workspaces: Array<DashboardWorkspace>;
	invitations: Array<DashboardWorkspaceInvitation>;
	canManageFleet: boolean;
}

export interface DashboardProjectSummary {
	project: DashboardProject;
	environmentCount: number;
	/** The environment a project opens on; absent when it has none. */
	landingEnvironment?: { id: string; name: string };
	services: Array<{ id: string; name: string; online: boolean }>;
}

export interface DashboardWorkspaceMember {
	userId: string;
	role: DashboardWorkspaceRole;
	joinedAt?: string;
	email?: string;
	githubLogin?: string;
}

export type DashboardDeletableKind = "project" | "environment" | "volume";

export type DashboardRestorableKind =
	| "project"
	| "environment"
	| "service"
	| "domain";

export type DashboardDeletedResourceKind = DashboardRestorableKind | "volume";

/** One tombstoned resource on the recently deleted view. `deletedWith` names the nearest tombstoned ancestor. */
export interface DashboardDeletedResource {
	kind: DashboardDeletedResourceKind;
	id: string;
	name: string;
	projectId: string;
	projectName: string;
	environmentName?: string;
	serviceName?: string;
	deletion: DashboardDeletionState;
	deletedWith?: {
		kind: DashboardDeletedResourceKind;
		id: string;
		name: string;
	};
}

export interface DashboardProjectSettings {
	project: DashboardProject;
	/** Every workspace the user belongs to, for moving the project. */
	workspaces: Array<DashboardWorkspace>;
	environments: Array<{
		environment: DashboardEnvironment;
		volumes: Array<DashboardVolume>;
	}>;
}

export interface DashboardGitHubAccount {
	providerSubject: string;
	login: string;
	primaryEmail: string;
	tokenType: string;
	scope: string;
}

export interface StoredDashboardGitHubAccount extends DashboardGitHubAccount {
	accessToken: string;
	tokenVersion: number;
	accessTokenExpiresAt?: Date;
	refreshToken?: string;
	refreshTokenExpiresAt?: Date;
}

export interface DashboardOnboardingDraft {
	projectId: string;
	environmentId: string;
	serviceId: string;
	repositorySelector: string;
	trackedRef: string;
	builder: string;
	dockerfilePath: string;
	contextDir: string;
	hostname: string;
}

export interface GitHubUserRepository {
	owner: string;
	name: string;
	fullName: string;
	private: boolean;
	defaultBranch: string;
}

export {
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
} from "./defaults";

export interface DashboardServicePosition {
	x: number;
	y: number;
}

export interface CreateServiceFastResult {
	project: DashboardProject;
	environment: DashboardEnvironment;
	service: DashboardServiceRecord;
	serviceStatus: DashboardServiceStatus | null;
	onboarding: DashboardOnboardingDraft;
}

export interface DashboardHomeState {
	user: DashboardUser;
	canManageFleet?: boolean;
	githubAccount?: DashboardGitHubAccount;
	onboarding: DashboardOnboardingDraft;
	repositories: Array<GitHubUserRepository>;
	githubLoginURL?: string;
	githubInstallURL?: string;
	publicBaseURL: string;
	localIngressBaseURL?: string;
	ingressTargetHost: string;
	localDomainSuffix?: string;
	/** The workspace that owns the open project. */
	workspace?: DashboardWorkspace;
	project?: DashboardProject;
	/** Live projects in the open project's workspace, for switching. */
	projects: Array<DashboardProject>;
	environments: Array<DashboardEnvironment>;
	environment?: DashboardEnvironment;
	services: Array<DashboardServiceRecord>;
	/** Live volumes in the selected environment. */
	volumes: Array<DashboardVolume>;
	servicesRevision: string;
	selectedServiceId: string | null;
	domainBindings: Array<DashboardDomainBinding>;
	controlPlaneReachable: boolean;
	controlPlaneError?: string;
}

export interface DevLoginIdentity {
	id: string;
	email: string;
}

export interface GitHubAppUserAuthConfig {
	appId: string;
	clientId: string;
	clientSecret: string;
	authorizationBaseURL: string;
	apiBaseURL: string;
}

export type DashboardRuntimeProfile = "development" | "production";

export interface DashboardConfig {
	installationID?: string;
	recoveryGeneration?: string;
	recoveryClusterID?: string;
	recoveryPaused?: boolean;
	recoveryCheckpoints?: boolean;
	profile: DashboardRuntimeProfile;
	sessionCookieName: string;
	refreshCookieName: string;
	authStateCookieName: string;
	publicBaseURL: string;
	localIngressBaseURL?: string;
	devUsers: Array<DevLoginIdentity>;
	sessionMaxAgeSeconds: number;
	jwtSecret: string;
	/** Previous session signing secret during rotation overlap. Verifies only, never signs. */
	jwtSecretPrevious?: string;
	githubInstallURL?: string;
	operatorGitHubLogin?: string;
	ingressTargetHost: string;
	localDomainSuffix?: string;
	github?: GitHubAppUserAuthConfig;
}

export interface DashboardSession {
	user: DashboardUser;
}
