import type {
	DashboardEnvironment,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

import { newestServiceRecord } from "./service-record-order";

const PENDING_CREATED_SERVICE_TTL_MS = 60_000;

type PendingCreatedService = {
	service: DashboardServiceRecord;
	environment: DashboardEnvironment;
	expiresAt: number;
};

// Survives the `/` → `/environments/:id` remount that happens when the first
// service materialises an environment. A component ref is wiped by that
// remount, which would close the service panel we just opened.
let pendingCreatedService: PendingCreatedService | null = null;

export function rememberPendingCreatedService(
	service: DashboardServiceRecord,
	environment: DashboardEnvironment,
) {
	pendingCreatedService = {
		service,
		environment,
		expiresAt: Date.now() + PENDING_CREATED_SERVICE_TTL_MS,
	};
}

export function clearPendingCreatedService(serviceId?: string) {
	if (!pendingCreatedService) return;
	if (serviceId && pendingCreatedService.service.id !== serviceId) return;
	pendingCreatedService = null;
}

export function readPendingCreatedService(): PendingCreatedService | null {
	const pending = pendingCreatedService;
	if (!pending) return null;
	if (Date.now() > pending.expiresAt) {
		pendingCreatedService = null;
		return null;
	}
	return pending;
}

export function withPendingCreatedService(
	list: Array<DashboardServiceRecord>,
): Array<DashboardServiceRecord> {
	const pending = readPendingCreatedService();
	if (!pending) return list;
	// Keep pending until the user closes the panel. A live snapshot that
	// already includes the service used to consume it, then `/` redirected
	// and remounted DashboardPage with nothing selected.
	if (list.some((service) => service.id === pending.service.id)) {
		return list;
	}
	return [...list, pending.service];
}

export function hydrateStateWithPendingCreated(
	state: DashboardHomeState,
): DashboardHomeState {
	const pending = readPendingCreatedService();
	if (!pending) return state;
	const pendingEnvironment = !state.environment
		? pending.environment
		: undefined;
	const pendingEnvironments = pendingEnvironment
		? state.environments.some((entry) => entry.id === pendingEnvironment.id)
			? state.environments
			: [...state.environments, pendingEnvironment]
		: state.environments;
	return {
		...state,
		environment: state.environment ?? pendingEnvironment,
		environments: pendingEnvironments,
		services: withPendingCreatedService(state.services),
	};
}

export function resetDashboardPageTestState() {
	pendingCreatedService = null;
}

export function mergeHomeStatusService(
	current: DashboardHomeState,
	status: DashboardServiceStatus,
	selectedId: string | null,
): DashboardHomeState {
	const incomingService = status.service;
	const existing = current.services.find(
		(entry) => entry.id === incomingService.id,
	);
	const mergedService = newestServiceRecord(existing, incomingService);
	return {
		...current,
		services: current.services.map((entry) =>
			entry.id === mergedService.id ? mergedService : entry,
		),
		service:
			current.service?.id === mergedService.id
				? mergedService
				: current.service,
		serviceStatus:
			selectedId === mergedService.id
				? { ...status, service: mergedService }
				: current.serviceStatus,
	};
}

export function mergeHomeEnvironmentServices(
	current: DashboardHomeState,
	nextServices: Array<DashboardServiceRecord>,
): DashboardHomeState {
	const currentByID = new Map(
		current.services.map((service) => [service.id, service]),
	);
	const mergedServices = withPendingCreatedService(
		nextServices.map((service) =>
			newestServiceRecord(currentByID.get(service.id), service),
		),
	);
	const selectedService =
		current.service &&
		mergedServices.find((service) => service.id === current.service?.id);
	const statusService =
		current.serviceStatus &&
		mergedServices.find(
			(service) => service.id === current.serviceStatus?.service.id,
		);
	return {
		...current,
		services: mergedServices,
		service: selectedService,
		serviceStatus:
			current.serviceStatus && statusService
				? { ...current.serviceStatus, service: statusService }
				: undefined,
	};
}

export function mergeHomeService(
	current: DashboardHomeState,
	service: DashboardServiceRecord,
): DashboardHomeState {
	const existing = current.services.find((entry) => entry.id === service.id);
	const merged = newestServiceRecord(existing, service);
	return {
		...current,
		services: current.services.map((entry) =>
			entry.id === merged.id ? merged : entry,
		),
		service: current.service?.id === merged.id ? merged : current.service,
		serviceStatus:
			current.serviceStatus?.service.id === merged.id
				? { ...current.serviceStatus, service: merged }
				: current.serviceStatus,
	};
}
