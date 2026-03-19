import {
	createDashboardService,
	type DashboardConfig,
	type DashboardProject,
	type DashboardService,
	type DashboardStore,
	type DashboardUser,
	type PlatformGateway,
	type SessionCookies,
} from "#/lib/dashboard-core.server";

interface SessionRecord {
	id: string;
	userID: string;
	expiresAt: Date;
}

export interface DashboardTestHarness {
	config: DashboardConfig;
	service: DashboardService;
	store: DashboardStore;
	platform: FakePlatformGateway;
	cookies: FakeSessionCookies;
}

export interface FakePlatformGateway extends PlatformGateway {
	ensurePrincipalCalls: Array<DashboardUser>;
	listProjectsCalls: Array<DashboardUser>;
	createProjectCalls: Array<{ user: DashboardUser; name: string }>;
	projects: Array<DashboardProject>;
	errors: {
		ensurePrincipal?: Error;
		listProjects?: Error;
		createProject?: Error;
	};
}

export interface FakeSessionCookies extends SessionCookies {
	values: Map<string, string>;
}

export function createDashboardTestHarness(
	overrides: Partial<DashboardConfig> = {},
): DashboardTestHarness {
	const config: DashboardConfig = {
		sessionCookieName: "dashboard_session",
		publicBaseURL: "https://dashboard.example.test",
		devUsers: [{ subject: "user-1", email: "user@example.com" }],
		sessionMaxAgeSeconds: 7 * 24 * 60 * 60,
		...overrides,
	};

	const users = new Map<string, DashboardUser>();
	const sessions = new Map<string, SessionRecord>();
	let nextUserID = 1;
	let nextSessionID = 1;
	const now = new Date("2026-03-18T12:00:00Z");

	const store: DashboardStore = {
		async ensureInitialized(): Promise<void> {},
		async upsertUser(subject, email): Promise<DashboardUser> {
			const existing = users.get(subject);
			if (existing) {
				const updated = { ...existing, email };
				users.set(subject, updated);
				return updated;
			}
			const user = {
				id: `user-${nextUserID++}`,
				subject,
				email,
			};
			users.set(subject, user);
			return user;
		},
		async createSession(sessionId, userID, expiresAt): Promise<void> {
			sessions.set(sessionId, { id: sessionId, userID, expiresAt });
		},
		async deleteSession(sessionId): Promise<void> {
			sessions.delete(sessionId);
		},
		async getSession(
			sessionId,
			currentTime,
		): Promise<{
			sessionId: string;
			user: DashboardUser;
		} | null> {
			const session = sessions.get(sessionId);
			if (!session || session.expiresAt <= currentTime) {
				return null;
			}
			const user = [...users.values()].find(
				(item) => item.id === session.userID,
			);
			if (!user) {
				return null;
			}
			return { sessionId, user };
		},
	};

	const platform: FakePlatformGateway = {
		ensurePrincipalCalls: [],
		listProjectsCalls: [],
		createProjectCalls: [],
		projects: [],
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
	};

	const cookies: FakeSessionCookies = {
		values: new Map<string, string>(),
		get(name) {
			return cookies.values.get(name);
		},
		set(name, value) {
			cookies.values.set(name, value);
		},
		delete(name) {
			cookies.values.delete(name);
		},
	};

	const service = createDashboardService(config, {
		store,
		platform,
		cookies,
		now: () => now,
		randomUUID: () => `session-${nextSessionID++}`,
	});

	return {
		config,
		service,
		store,
		platform,
		cookies,
	};
}
