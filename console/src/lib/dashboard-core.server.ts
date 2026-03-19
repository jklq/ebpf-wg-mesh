export interface DashboardUser {
	id: string;
	subject: string;
	email: string;
}

export type DashboardProjectKind = "user" | "managed";

export interface DashboardProject {
	id: string;
	name: string;
	kind: DashboardProjectKind;
	systemKey?: string;
}

export interface DashboardHomeState {
	user: DashboardUser;
	projects: Array<DashboardProject>;
	controlPlaneReachable: boolean;
	controlPlaneError?: string;
}

export interface DevLoginIdentity {
	subject: string;
	email: string;
}

export interface DashboardConfig {
	sessionCookieName: string;
	publicBaseURL: string;
	devUsers: Array<DevLoginIdentity>;
	sessionMaxAgeSeconds: number;
}

interface DashboardSession {
	sessionId: string;
	user: DashboardUser;
}

export interface DashboardStore {
	ensureInitialized(): Promise<void>;
	upsertUser(subject: string, email: string): Promise<DashboardUser>;
	createSession(
		sessionId: string,
		userID: string,
		expiresAt: Date,
	): Promise<void>;
	deleteSession(sessionId: string): Promise<void>;
	getSession(sessionId: string, now: Date): Promise<DashboardSession | null>;
}

export interface PlatformGateway {
	ensurePrincipal(user: DashboardUser): Promise<void>;
	listProjects(user: DashboardUser): Promise<Array<DashboardProject>>;
	createProject(user: DashboardUser, name: string): Promise<DashboardProject>;
}

export interface SessionCookieOptions {
	httpOnly: boolean;
	path: string;
	sameSite: "lax";
	secure: boolean;
	expires: Date;
}

export interface SessionCookies {
	get(name: string): string | undefined;
	set(name: string, value: string, options: SessionCookieOptions): void;
	delete(name: string, options: { path: string }): void;
}

export interface DashboardDependencies {
	store: DashboardStore;
	platform: PlatformGateway;
	cookies: SessionCookies;
	now?: () => Date;
	randomUUID?: () => string;
}

export interface DashboardService {
	listDevLogins(): Array<DevLoginIdentity>;
	completeDevLogin(input: {
		subject: string;
		email: string;
		redirectTo?: string;
	}): Promise<string>;
	loadDashboardHome(): Promise<DashboardHomeState | null>;
	createProjectFromSession(name: string): Promise<DashboardProject>;
	clearSession(): Promise<void>;
}

export function createDashboardService(
	config: DashboardConfig,
	deps: DashboardDependencies,
): DashboardService {
	const now = deps.now ?? (() => new Date());
	const createID = deps.randomUUID ?? (() => crypto.randomUUID());

	async function currentSession(): Promise<DashboardSession | null> {
		await deps.store.ensureInitialized();
		const sessionId = deps.cookies.get(config.sessionCookieName);
		if (!sessionId) {
			return null;
		}
		const session = await deps.store.getSession(sessionId, now());
		if (session) {
			return session;
		}
		await deps.store.deleteSession(sessionId);
		deps.cookies.delete(config.sessionCookieName, { path: "/" });
		return null;
	}

	async function requireSession(): Promise<DashboardSession> {
		const session = await currentSession();
		if (!session) {
			throw new Error("authentication required");
		}
		return session;
	}

	return {
		listDevLogins(): Array<DevLoginIdentity> {
			return config.devUsers;
		},

		async completeDevLogin(input): Promise<string> {
			const subject = input.subject.trim();
			const email = input.email.trim();
			if (subject === "" || email === "") {
				throw new Error("subject and email are required");
			}

			await deps.store.ensureInitialized();
			const user = await deps.store.upsertUser(subject, email);
			const sessionId = createID();
			const expiresAt = new Date(
				now().getTime() + config.sessionMaxAgeSeconds * 1000,
			);

			await deps.store.createSession(sessionId, user.id, expiresAt);
			deps.cookies.set(
				config.sessionCookieName,
				sessionId,
				sessionCookieOptions(config, expiresAt),
			);

			try {
				await deps.platform.ensurePrincipal(user);
			} catch {
				// Keep the app session even if control-plane projection is temporarily unavailable.
			}

			return sanitizeRedirect(input.redirectTo);
		},

		async loadDashboardHome(): Promise<DashboardHomeState | null> {
			const session = await currentSession();
			if (!session) {
				return null;
			}
			try {
				await deps.platform.ensurePrincipal(session.user);
				const projects = await deps.platform.listProjects(session.user);
				return {
					user: session.user,
					projects,
					controlPlaneReachable: true,
				};
			} catch (error) {
				return {
					user: session.user,
					projects: [],
					controlPlaneReachable: false,
					controlPlaneError: formatError(error),
				};
			}
		},

		async createProjectFromSession(name: string): Promise<DashboardProject> {
			const session = await requireSession();
			const projectName = name.trim();
			if (projectName === "") {
				throw new Error("project name is required");
			}
			await deps.platform.ensurePrincipal(session.user);
			return deps.platform.createProject(session.user, projectName);
		},

		async clearSession(): Promise<void> {
			const sessionId = deps.cookies.get(config.sessionCookieName);
			if (sessionId) {
				await deps.store.ensureInitialized();
				await deps.store.deleteSession(sessionId);
			}
			deps.cookies.delete(config.sessionCookieName, { path: "/" });
		},
	};
}

export function sessionCookieOptions(
	config: DashboardConfig,
	expiresAt: Date,
): SessionCookieOptions {
	return {
		httpOnly: true,
		path: "/",
		sameSite: "lax",
		secure: config.publicBaseURL.startsWith("https://"),
		expires: expiresAt,
	};
}

export function sanitizeRedirect(value?: string): string {
	if (!value || !value.startsWith("/")) {
		return "/";
	}
	return value;
}

export function formatError(error: unknown): string {
	if (error && typeof error === "object" && "message" in error) {
		return String(error.message);
	}
	return "unknown error";
}

export function parseIdentifier(raw: string): string {
	if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(raw)) {
		throw new Error(`invalid dashboard schema identifier: ${raw}`);
	}
	return raw;
}

export function parseDevUsers(raw: string): Array<DevLoginIdentity> {
	if (raw.trim() === "") {
		return [];
	}
	return raw
		.split(";")
		.map((entry) => entry.trim())
		.filter((entry) => entry !== "")
		.map((entry) => {
			const [subject, email] = entry.split(":", 2);
			return {
				subject: (subject ?? "").trim(),
				email: (email ?? "").trim(),
			};
		})
		.filter((entry) => entry.subject !== "" && entry.email !== "");
}
