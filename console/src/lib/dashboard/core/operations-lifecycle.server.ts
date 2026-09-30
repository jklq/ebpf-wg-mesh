import { requireSession } from "#/lib/dashboard/core/auth.server";
import type { DashboardRuntime } from "#/lib/dashboard/core/runtime.server";
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
import { PlatformService } from "#/lib/platform-gen/platform_pb";
import { dateMillis } from "#/lib/time";

export async function loadProjectSettingsFromSession(
	runtime: DashboardRuntime,
	projectId: string,
): Promise<DashboardProjectSettings> {
	const session = await requireSession(runtime);
	const [project, environments] = await Promise.all([
		runtime.platform.call(PlatformService.method.getProject, session.user, {
			projectId: projectId,
		}),
		runtime.platform
			.call(PlatformService.method.listEnvironments, session.user, {
				projectId: projectId,
			})
			.then((response) => response.environments ?? []),
	]);
	return {
		project,
		environments: await Promise.all(
			sortEnvironments(environments).map(async (environment) => ({
				environment,
				volumes: await runtime.platform
					.call(PlatformService.method.listVolumes, session.user, {
						environmentId: environment.id,
					})
					.then((response) => response.volumes ?? []),
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
	const environments = await runtime.platform
		.call(PlatformService.method.listEnvironments, session.user, {
			projectId: projectId,
		})
		.then((response) => response.environments ?? []);
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
	return runtime.platform.call(
		PlatformService.method.updateProjectLogRetention,
		session.user,
		{
			projectId: input.projectId,
			logRetentionDays: days,
		},
	);
}

export async function previewDeletionFromSession(
	runtime: DashboardRuntime,
	input: { kind: DashboardDeletableKind; id: string },
): Promise<DashboardDeletionPreview> {
	const session = await requireSession(runtime);
	switch (input.kind) {
		case "project":
			return runtime.platform.call(
				PlatformService.method.previewProjectDeletion,
				session.user,
				{ projectId: input.id },
			);
		case "environment":
			return runtime.platform.call(
				PlatformService.method.previewEnvironmentDeletion,
				session.user,
				{ environmentId: input.id },
			);
		case "volume":
			return runtime.platform.call(
				PlatformService.method.previewVolumeDeletion,
				session.user,
				{ volumeId: input.id },
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
			await runtime.platform.call(
				PlatformService.method.deleteProject,
				session.user,
				{
					projectId: input.id,
					confirmationName,
				},
			);
			return;
		case "environment":
			await runtime.platform.call(
				PlatformService.method.deleteEnvironment,
				session.user,
				{
					environmentId: input.id,
					confirmationName,
				},
			);
			return;
		case "volume":
			await runtime.platform.call(
				PlatformService.method.deleteVolume,
				session.user,
				{
					volumeId: input.id,
					confirmationName,
				},
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
			await runtime.platform.call(
				PlatformService.method.restoreProject,
				session.user,
				{ projectId: input.id },
			);
			return;
		case "environment":
			await runtime.platform.call(
				PlatformService.method.restoreEnvironment,
				session.user,
				{ environmentId: input.id },
			);
			return;
		case "service":
			await runtime.platform.call(
				PlatformService.method.restoreService,
				session.user,
				{ serviceId: input.id },
			);
			return;
		case "domain":
			await runtime.platform.call(
				PlatformService.method.restoreDomainBinding,
				session.user,
				{ hostname: input.id },
			);
			return;
	}
}

/** Collects every tombstoned resource the user can see, newest first. Deleted projects list without contents. */
export async function loadRecentlyDeletedFromSession(
	runtime: DashboardRuntime,
): Promise<Array<DashboardDeletedResource>> {
	const session = await requireSession(runtime);
	const projects = await runtime.platform
		.call(PlatformService.method.listProjects, session.user, {
			includeDeleted: true,
		})
		.then((response) => response.projects ?? []);
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
				(dateMillis(right.deletion.deletedAt) ?? 0) -
				(dateMillis(left.deletion.deletedAt) ?? 0),
		);
}

async function deletedInProject(
	runtime: DashboardRuntime,
	user: DashboardUser,
	project: DashboardProject,
): Promise<Array<DashboardDeletedResource>> {
	const environments = await runtime.platform
		.call(PlatformService.method.listEnvironments, user, {
			projectId: project.id,
			...{ includeDeleted: true },
		})
		.then((response) => response.environments ?? []);
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
				runtime.platform.call(PlatformService.method.listServices, user, {
					environmentId: environment.id,
					...{
						includeDeleted: true,
					},
				}),
				runtime.platform
					.call(PlatformService.method.listVolumes, user, {
						environmentId: environment.id,
						...{ includeDeleted: true },
					})
					.then((response) => response.volumes ?? []),
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
					const domains = await runtime.platform
						.call(PlatformService.method.listDomainBindings, user, {
							serviceId: service.id,
							includeDeleted: true,
						})
						.then((response) => response.bindings ?? []);
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
