import {
	type DashboardRuntime,
	listGitHubRepositories,
	nextGeneratedProjectName,
	platformCall,
	refreshGitHubAccount,
	storeCall,
	toGitHubApiError,
} from "#/lib/dashboard/core/runtime.server";
import {
	type DashboardGitHubAccount,
	type DashboardProject,
	type DashboardServicePosition,
	type DashboardServiceRecord,
	type DashboardServiceSpec,
	type DashboardSourceSpec,
	type DashboardUser,
	DashboardValidationError,
	DEFAULT_SERVICE_CPU_MILLIS,
	DEFAULT_SERVICE_MEMORY_MEBIBYTES,
	type FleetAgentInput,
	GitHubApiError,
	type GitHubUserRepository,
	type StoredDashboardGitHubAccount,
} from "#/lib/dashboard/core/types.server";

export function normalizeFleetAgentInput(
	input: FleetAgentInput,
): FleetAgentInput {
	return {
		agentId: input.agentId.trim(),
		name: input.name.trim(),
		region: input.region.trim().toLowerCase(),
		zone: input.zone.trim().toLowerCase(),
		failureDomain: input.failureDomain.trim().toLowerCase(),
		reservedCpuMillis: Number(input.reservedCpuMillis),
		reservedMemoryMebibytes: Number(input.reservedMemoryMebibytes),
	};
}

export async function loadGitHubCatalog(
	runtime: DashboardRuntime,
	userID: string,
	githubAccount: StoredDashboardGitHubAccount | null,
): Promise<{
	githubAccount: StoredDashboardGitHubAccount | null;
	repositories: Array<GitHubUserRepository>;
}> {
	if (!githubAccount?.accessToken) {
		return { githubAccount, repositories: [] };
	}
	try {
		return {
			githubAccount,
			repositories: await listGitHubRepositories(
				runtime,
				githubAccount.accessToken,
			),
		};
	} catch (cause) {
		const err = toGitHubApiError("listRepositories", cause);
		if (
			err instanceof GitHubApiError &&
			err.status === 401 &&
			githubAccount.refreshToken
		) {
			try {
				const refreshed = await refreshGitHubAccount(
					runtime,
					userID,
					githubAccount,
				);
				return {
					githubAccount: refreshed,
					repositories: await listGitHubRepositories(
						runtime,
						refreshed.accessToken,
					),
				};
			} catch {
				return { githubAccount, repositories: [] };
			}
		}
		return { githubAccount, repositories: [] };
	}
}

export function publicGitHubAccount(
	account: StoredDashboardGitHubAccount | null,
): DashboardGitHubAccount | undefined {
	if (!account) {
		return undefined;
	}
	return {
		providerSubject: account.providerSubject,
		login: account.login,
		primaryEmail: account.primaryEmail,
		tokenType: account.tokenType,
		scope: account.scope,
	};
}

export async function requireGitHubRepositoryAccess(
	runtime: DashboardRuntime,
	userID: string,
	repositorySelector: string,
): Promise<string> {
	const account = await storeCall(runtime, "getGitHubAccount", (store) =>
		store.getGitHubAccount(userID),
	);
	if (!account && runtime.config.devUsers.some((user) => user.id === userID)) {
		return "";
	}
	const catalog = await loadGitHubCatalog(runtime, userID, account);
	const expected = repositorySelector.toLowerCase();
	if (
		!catalog.repositories.some(
			(repository) => repository.fullName.toLowerCase() === expected,
		)
	) {
		throw new DashboardValidationError({
			message: "The signed-in GitHub account cannot access this repository.",
		});
	}
	return catalog.githubAccount?.accessToken ?? "";
}

export async function repositoryProject(
	runtime: DashboardRuntime,
	user: DashboardUser,
	preferredProjectID: string,
): Promise<DashboardProject> {
	const projects = await platformCall(runtime, "listProjects", (platform) =>
		platform.listProjects(user),
	);
	const existing = projects.find(
		(project) => project.id === preferredProjectID,
	);
	if (existing) {
		return existing;
	}
	return platformCall(runtime, "createProject", (platform) =>
		platform.createProject(
			user,
			nextGeneratedProjectName(projects, runtime.randomUUID()),
		),
	);
}

export function buildServiceSpec(
	source: DashboardSourceSpec,
	recommendedPorts: number[],
	cpuMillis?: number,
	memoryMebibytes?: number,
): DashboardServiceSpec {
	const seen = new Set<number>();
	const ports = recommendedPorts
		.filter((port) => Number.isInteger(port) && port >= 1 && port <= 65535)
		.filter((port) => {
			if (seen.has(port)) {
				return false;
			}
			seen.add(port);
			return true;
		})
		.map((port, index) => ({
			port,
			primary: index === 0,
		}));
	return {
		source,
		desiredReplicaCount: 1,
		runtime: {
			env: {},
			cpuMillis: normalizeResource(
				cpuMillis,
				DEFAULT_SERVICE_CPU_MILLIS,
				"CPU request",
			),
			memoryMebibytes: normalizeResource(
				memoryMebibytes,
				DEFAULT_SERVICE_MEMORY_MEBIBYTES,
				"memory request",
			),
			ports,
		},
	};
}

export function normalizeResource(
	value: number | undefined,
	fallback: number,
	label: string,
): number {
	const normalized = value ?? fallback;
	if (!Number.isSafeInteger(normalized) || normalized < fallback) {
		throw new DashboardValidationError({
			message: `${label} must be at least ${fallback}`,
		});
	}
	return normalized;
}

export function normalizeRuntimeEnv(
	env: Record<string, string> | undefined,
): Record<string, string> {
	if (!env) {
		return {};
	}
	const normalized: Record<string, string> = {};
	for (const [rawKey, rawValue] of Object.entries(env)) {
		const key = rawKey.trim();
		if (key === "") {
			continue;
		}
		if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
			throw new DashboardValidationError({
				message: `Invalid environment variable name: ${key}`,
			});
		}
		normalized[key] = String(rawValue);
	}
	return normalized;
}

export function applyServicePositions(
	services: Array<DashboardServiceRecord>,
	positions: Record<string, DashboardServicePosition>,
): Array<DashboardServiceRecord> {
	return services.map((service) => {
		const position = positions[service.id];
		return position ? { ...service, layoutPosition: position } : service;
	});
}

export function normalizeServicePosition(
	position: DashboardServicePosition,
): DashboardServicePosition {
	const x = Math.round(position.x);
	const y = Math.round(position.y);
	if (!Number.isFinite(x) || !Number.isFinite(y)) {
		throw new DashboardValidationError({
			message: "service position must be finite",
		});
	}
	return { x, y };
}
