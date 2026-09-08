import { vi } from "vitest";

import type {
	DashboardHomeState,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

export class MockEventSource {
	static instances: MockEventSource[] = [];

	readonly url: string;
	readonly close = vi.fn();
	readonly listeners = new Map<
		string,
		Array<(event: MessageEvent<string>) => void>
	>();
	readonly addEventListener = vi.fn(
		(type: string, listener: (event: MessageEvent<string>) => void) => {
			this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
		},
	);
	onerror: (() => void) | null = null;

	constructor(url: string) {
		this.url = url;
		MockEventSource.instances.push(this);
	}

	emit(type: string, data: unknown) {
		for (const listener of this.listeners.get(type) ?? []) {
			listener(new MessageEvent(type, { data: JSON.stringify(data) }));
		}
	}
}

export function stubDashboardLayoutMetrics() {
	Object.defineProperty(HTMLElement.prototype, "clientWidth", {
		configurable: true,
		value: 1000,
	});
	Object.defineProperty(HTMLElement.prototype, "clientHeight", {
		configurable: true,
		value: 800,
	});
	Object.defineProperty(window, "innerWidth", {
		configurable: true,
		value: 1000,
	});
	Object.defineProperty(window, "innerHeight", {
		configurable: true,
		value: 848,
	});
}

export function dashboardState(
	service: DashboardServiceRecord,
	overrides: Partial<DashboardHomeState> = {},
): DashboardHomeState {
	return {
		user: {
			id: "user-1",
			email: "user@example.com",
		},
		project: {
			id: "project-1",
			name: "test-project",
			kind: "PROJECT_KIND_USER",
		},
		environments: [
			{
				id: "environment-1",
				projectId: "project-1",
				name: "Production",
				kind: "persistent",
				isProduction: true,
			},
		],
		environment: {
			id: "environment-1",
			projectId: "project-1",
			name: "Production",
			kind: "persistent",
			isProduction: true,
		},
		onboarding: {
			projectId: "project-1",
			environmentId: "environment-1",
			serviceId: service.id,
			repositorySelector: "octocat/hello",
			trackedRef: "main",
			dockerfilePath: "Dockerfile",
			contextDir: ".",
			hostname: "",
		},
		repositories: [],
		services: [service],
		service,
		serviceStatus: undefined,
		githubInstallURL:
			"https://github.example.test/apps/platform/installations/new",
		publicBaseURL: "https://dashboard.example.test",
		localIngressBaseURL: undefined,
		ingressTargetHost: "platform.example.test",
		localDomainSuffix: undefined,
		domainBindings: [],
		controlPlaneReachable: true,
		...overrides,
	};
}

export function emptyState(): DashboardHomeState {
	return dashboardState(serviceRecord(), {
		services: [],
		service: undefined,
		environment: undefined,
		githubAccount: {
			providerSubject: "1",
			login: "octocat",
			primaryEmail: "octocat@example.com",
			tokenType: "bearer",
			scope: "repo",
		},
		repositories: [
			{
				owner: "octocat",
				name: "hello",
				fullName: "octocat/hello",
				private: false,
				defaultBranch: "main",
			},
		],
	});
}

export function serviceRecord(
	overrides: Partial<DashboardServiceRecord> = {},
): DashboardServiceRecord {
	return {
		id: "service-1",
		environmentId: "environment-1",
		projectId: "project-1",
		name: "hello",
		spec: {
			source: {
				provider: "github",
				repositorySelector: "octocat/hello",
				trackedRef: "main",
			},
			runtime: { env: {}, cpuMillis: 250, memoryMebibytes: 256, ports: [] },
		},
		...overrides,
	};
}

export function unappliedChange(
	id: string,
	section: string,
	field: string,
	currentValue: string,
	newValue: string,
): NonNullable<DashboardServiceRecord["unappliedChanges"]>[number] {
	return {
		id,
		section,
		field,
		path: id,
		action: currentValue
			? "SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE"
			: "SERVICE_UNAPPLIED_CHANGE_ACTION_ADD",
		currentValue,
		newValue,
	};
}

export function deferred<T>() {
	let resolve!: (value: T) => void;
	let reject!: (error: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}
