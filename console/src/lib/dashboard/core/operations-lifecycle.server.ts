import { requireSession } from "#/lib/dashboard/core/auth.server";
import {
	type DashboardRuntime,
	platformCall,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardDeletableKind,
	type DashboardDeletedResource,
	type DashboardDeletionPreview,
	type DashboardEnvironment,
	type DashboardProject,
	type DashboardProjectSettings,
	type DashboardRestorableKind,
	type DashboardUser,
	DashboardValidationError,
} from "#/lib/dashboard/core/types.server";

export async function loadProjectSettingsFromSession(
	runtime: DashboardRuntime,
	projectId: string,
): Promise<DashboardProjectSettings> {
	const session = await requireSession(runtime);
	const [project, environments] = await Promise.all([
		platformCall(runtime, "getProject", (platform) =>
			platform.getProject(session.user, projectId),
		),
		platformCall(runtime, "listEnvironments", (platform) =>
			platform.listEnvironments(session.user, projectId),
		),
	]);
	return {
		project,
		environments: await Promise.all(
			sortEnvironments(environments).map(async (environment) => ({
				environment,
				volumes: await platformCall(runtime, "listVolumes", (platform) =>
					platform.listVolumes(session.user, environment.id),
				),
			})),
		),
	};
}

/** The environment a project opens on: production, else the first one. */
export async function projectLandingEnvironmentFromSession(
	runtime: DashboardRuntime,
	projectId: string,
): Promise<string | null> {
	const session = await requireSession(runtime);
	const environments = await platformCall(
		runtime,
		"listEnvironments",
		(platform) => platform.listEnvironments(session.user, projectId),
	);
	return sortEnvironments(environments)[0]?.id ?? null;
}

export async function updateProjectLogRetentionFromSession(
	runtime: DashboardRuntime,
	input: { projectId: string; logRetentionDays: number },
): Promise<DashboardProject> {
	const session = await requireSession(runtime);
	const days = input.logRetentionDays;
	if (!Number.isInteger(days) || days < 0 || days > 90) {
		throw new DashboardValidationError({
			message: "Log retention must be 1–90 days, or the platform default.",
		});
	}
	return platformCall(runtime, "updateProjectLogRetention", (platform) =>
		platform.updateProjectLogRetention(session.user, {
			projectId: input.projectId,
			logRetentionDays: days,
		}),
	);
}

export async function previewDeletionFromSession(
	runtime: DashboardRuntime,
	input: { kind: DashboardDeletableKind; id: string },
): Promise<DashboardDeletionPreview> {
	const session = await requireSession(runtime);
	switch (input.kind) {
		case "project":
			return platformCall(runtime, "previewProjectDeletion", (platform) =>
				platform.previewProjectDeletion(session.user, input.id),
			);
		case "environment":
			return platformCall(runtime, "previewEnvironmentDeletion", (platform) =>
				platform.previewEnvironmentDeletion(session.user, input.id),
			);
		case "volume":
			return platformCall(runtime, "previewVolumeDeletion", (platform) =>
				platform.previewVolumeDeletion(session.user, input.id),
			);
	}
}

export async function deleteResourceFromSession(
	runtime: DashboardRuntime,
	input: {
		kind: DashboardDeletableKind;
		id: string;
		confirmationName?: string;
	},
): Promise<void> {
	const session = await requireSession(runtime);
	const confirmationName = input.confirmationName ?? "";
	switch (input.kind) {
		case "project":
			return platformCall(runtime, "deleteProject", (platform) =>
				platform.deleteProject(session.user, {
					projectId: input.id,
					confirmationName,
				}),
			);
		case "environment":
			return platformCall(runtime, "deleteEnvironment", (platform) =>
				platform.deleteEnvironment(session.user, {
					environmentId: input.id,
					confirmationName,
				}),
			);
		case "volume":
			return platformCall(runtime, "deleteVolume", (platform) =>
				platform.deleteVolume(session.user, {
					volumeId: input.id,
					confirmationName,
				}),
			);
	}
}

export async function restoreResourceFromSession(
	runtime: DashboardRuntime,
	input: { kind: DashboardRestorableKind; id: string },
): Promise<void> {
	const session = await requireSession(runtime);
	switch (input.kind) {
		case "project":
			await platformCall(runtime, "restoreProject", (platform) =>
				platform.restoreProject(session.user, input.id),
			);
			return;
		case "environment":
			await platformCall(runtime, "restoreEnvironment", (platform) =>
				platform.restoreEnvironment(session.user, input.id),
			);
			return;
		case "service":
			await platformCall(runtime, "restoreService", (platform) =>
				platform.restoreService(session.user, input.id),
			);
			return;
		case "domain":
			await platformCall(runtime, "restoreDomainBinding", (platform) =>
				platform.restoreDomainBinding(session.user, input.id),
			);
			return;
	}
}

/**
 * Collects every tombstoned resource the user can see, newest first. Deleted
 * projects are listed without their contents: restoring the project brings
 * them back. Live projects are walked for tombstoned environments, services,
 * volumes, and domains.
 */
export async function loadRecentlyDeletedFromSession(
	runtime: DashboardRuntime,
): Promise<Array<DashboardDeletedResource>> {
	const session = await requireSession(runtime);
	const projects = await platformCall(runtime, "listProjects", (platform) =>
		platform.listProjects(session.user, { includeDeleted: true }),
	);
	const groups = await Promise.all(
		projects.map((project) =>
			project.deletion
				? Promise.resolve([
						{
							kind: "project" as const,
							id: project.id,
							name: project.name,
							projectId: project.id,
							projectName: project.name,
							deletion: project.deletion,
						},
					])
				: deletedInProject(runtime, session.user, project),
		),
	);
	return groups
		.flat()
		.sort(
			(left, right) =>
				(right.deletion.deletedAt?.getTime() ?? 0) -
				(left.deletion.deletedAt?.getTime() ?? 0),
		);
}

async function deletedInProject(
	runtime: DashboardRuntime,
	user: DashboardUser,
	project: DashboardProject,
): Promise<Array<DashboardDeletedResource>> {
	const environments = await platformCall(
		runtime,
		"listEnvironments",
		(platform) =>
			platform.listEnvironments(user, project.id, { includeDeleted: true }),
	);
	const base = { projectId: project.id, projectName: project.name };
	const perEnvironment = await Promise.all(
		environments.map(async (environment) => {
			const out: Array<DashboardDeletedResource> = [];
			const environmentTombstone = environment.deletion
				? {
						kind: "environment" as const,
						id: environment.id,
						name: environment.name,
					}
				: undefined;
			if (environment.deletion) {
				out.push({
					...base,
					kind: "environment",
					id: environment.id,
					name: environment.name,
					environmentName: environment.name,
					deletion: environment.deletion,
				});
			}
			const [services, volumes] = await Promise.all([
				platformCall(runtime, "listServices", (platform) =>
					platform.listServices(user, environment.id, {
						includeDeleted: true,
					}),
				),
				platformCall(runtime, "listVolumes", (platform) =>
					platform.listVolumes(user, environment.id, { includeDeleted: true }),
				),
			]);
			for (const volume of volumes) {
				if (!volume.deletion) continue;
				out.push({
					...base,
					kind: "volume",
					id: volume.id,
					name: volume.name,
					environmentName: environment.name,
					deletion: volume.deletion,
					deletedWith: environmentTombstone,
				});
			}
			const domainLists = await Promise.all(
				(services.services ?? []).map(async (service) => {
					const serviceTombstone = service.deletion
						? { kind: "service" as const, id: service.id, name: service.name }
						: undefined;
					const entries: Array<DashboardDeletedResource> = [];
					if (service.deletion) {
						entries.push({
							...base,
							kind: "service",
							id: service.id,
							name: service.name,
							environmentName: environment.name,
							deletion: service.deletion,
							deletedWith: environmentTombstone,
						});
					}
					const domains = await platformCall(
						runtime,
						"listDomainBindings",
						(platform) =>
							platform.listDomainBindings(user, {
								serviceId: service.id,
								includeDeleted: true,
							}),
					);
					for (const domain of domains) {
						if (!domain.deletion) continue;
						entries.push({
							...base,
							kind: "domain",
							id: domain.hostname,
							name: domain.hostname,
							environmentName: environment.name,
							serviceName: service.name,
							deletion: domain.deletion,
							deletedWith: serviceTombstone ?? environmentTombstone,
						});
					}
					return entries;
				}),
			);
			return [...out, ...domainLists.flat()];
		}),
	);
	return perEnvironment.flat();
}

function sortEnvironments(
	environments: Array<DashboardEnvironment>,
): Array<DashboardEnvironment> {
	return [...environments].sort(
		(left, right) =>
			Number(right.isProduction) - Number(left.isProduction) ||
			left.name.localeCompare(right.name),
	);
}
