import type {
	DashboardAllocationStatus,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

export interface ServicesSnapshot {
	services: Array<DashboardServiceRecord>;
	revision: number;
}

export interface ServiceStatusSnapshot {
	status: DashboardServiceStatus;
	revision: number;
}

export interface ServiceAllocations {
	allocation?: DashboardAllocationStatus;
	allocations?: Array<DashboardAllocationStatus>;
	revision: number;
}

export interface NormalizedDashboardState {
	base: Omit<
		DashboardHomeState,
		"services" | "servicesRevision" | "selectedServiceId"
	>;
	servicesById: Record<string, DashboardServiceRecord>;
	serviceOrder: Array<string>;
	servicesRevision: number;
	selectedServiceId: string | null;
	allocationsByServiceId: Record<string, ServiceAllocations>;
	statusRevisionByServiceId: Record<string, number>;
}

export function createNormalizedState(
	loader: DashboardHomeState,
	extra?: {
		services?: Array<DashboardServiceRecord>;
		selectedServiceId?: string | null | undefined;
	},
): NormalizedDashboardState {
	const services = extra?.services ?? loader.services;
	const byId: Record<string, DashboardServiceRecord> = {};
	const order: Array<string> = [];
	for (const service of services) {
		if (!byId[service.id]) order.push(service.id);
		byId[service.id] = service;
	}
	return {
		base: stripLoaderFields(loader),
		servicesById: byId,
		serviceOrder: order,
		servicesRevision: loader.servicesRevision,
		selectedServiceId:
			extra?.selectedServiceId !== undefined
				? extra.selectedServiceId
				: loader.selectedServiceId,
		allocationsByServiceId: {},
		statusRevisionByServiceId: {},
	};
}

export function selectServicesArray(
	state: NormalizedDashboardState,
): Array<DashboardServiceRecord> {
	return state.serviceOrder
		.map((id) => state.servicesById[id])
		.filter((service): service is DashboardServiceRecord => Boolean(service));
}

export function selectSelectedService(
	state: NormalizedDashboardState,
): DashboardServiceRecord | undefined {
	return state.selectedServiceId
		? state.servicesById[state.selectedServiceId]
		: undefined;
}

export function selectSelectedStatus(
	state: NormalizedDashboardState,
): DashboardServiceStatus | null {
	const service = selectSelectedService(state);
	if (!service) return null;
	const details = state.selectedServiceId
		? state.allocationsByServiceId[state.selectedServiceId]
		: undefined;
	if (!details) return null;
	return {
		service,
		allocation: details.allocation,
		allocations: details.allocations,
	};
}

export function applyServicesSnapshot(
	state: NormalizedDashboardState,
	snapshot: ServicesSnapshot,
	retained?: Array<DashboardServiceRecord>,
	force?: boolean,
): NormalizedDashboardState {
	if (!force && snapshot.revision <= state.servicesRevision) return state;
	const merged = mergeRetainingCreated(snapshot.services, retained);
	const byId: Record<string, DashboardServiceRecord> = {};
	const order: Array<string> = [];
	for (const service of merged) {
		if (!byId[service.id]) order.push(service.id);
		byId[service.id] = service;
	}
	const selectedKept =
		state.selectedServiceId && byId[state.selectedServiceId]
			? state.selectedServiceId
			: null;
	return {
		...state,
		servicesById: byId,
		serviceOrder: order,
		servicesRevision: snapshot.revision,
		selectedServiceId: selectedKept,
	};
}

export function applyLoaderState(
	state: NormalizedDashboardState,
	loader: DashboardHomeState,
	retained?: Array<DashboardServiceRecord>,
): NormalizedDashboardState {
	const next: NormalizedDashboardState = {
		...state,
		base: stripLoaderFields(loader),
	};
	if (loader.environment?.id !== state.base.environment?.id) {
		const reset: NormalizedDashboardState = {
			...next,
			selectedServiceId: loader.environment
				? loader.selectedServiceId
				: state.selectedServiceId,
		};
		return applyServicesSnapshot(
			reset,
			{ services: loader.services, revision: loader.servicesRevision },
			retained,
			true,
		);
	}
	if (loader.servicesRevision <= state.servicesRevision) {
		if (!retained || retained.length === 0) return next;
		const byId = { ...next.servicesById };
		const order = [...next.serviceOrder];
		for (const service of retained) {
			if (!byId[service.id]) {
				byId[service.id] = service;
				order.push(service.id);
			}
		}
		return {
			...next,
			servicesById: byId,
			serviceOrder: order,
			selectedServiceId:
				next.selectedServiceId && byId[next.selectedServiceId]
					? next.selectedServiceId
					: null,
		};
	}
	return applyServicesSnapshot(
		{ ...next, selectedServiceId: state.selectedServiceId },
		{ services: loader.services, revision: loader.servicesRevision },
		retained,
	);
}

export function upsertServiceRecord(
	state: NormalizedDashboardState,
	service: DashboardServiceRecord,
): NormalizedDashboardState {
	const exists = Boolean(state.servicesById[service.id]);
	return {
		...state,
		servicesById: { ...state.servicesById, [service.id]: service },
		serviceOrder: exists
			? state.serviceOrder
			: [...state.serviceOrder, service.id],
	};
}

export function applyServiceStatusSnapshot(
	state: NormalizedDashboardState,
	snapshot: ServiceStatusSnapshot,
): NormalizedDashboardState {
	const serviceId = snapshot.status.service.id;
	if (
		(snapshot.revision ?? 0) <=
		(state.statusRevisionByServiceId[serviceId] ?? -1)
	) {
		return state;
	}
	return {
		...upsertServiceRecord(state, snapshot.status.service),
		allocationsByServiceId: {
			...state.allocationsByServiceId,
			[serviceId]: {
				allocation: snapshot.status.allocation,
				allocations: snapshot.status.allocations,
				revision: snapshot.revision,
			},
		},
		statusRevisionByServiceId: {
			...state.statusRevisionByServiceId,
			[serviceId]: snapshot.revision,
		},
	};
}

export function applyMutationRecord(
	state: NormalizedDashboardState,
	service: DashboardServiceRecord,
	basisRevision: number,
): NormalizedDashboardState {
	if (basisRevision < state.servicesRevision) return state;
	return upsertServiceRecord(state, service);
}

export function applyMutationStatus(
	state: NormalizedDashboardState,
	status: DashboardServiceStatus,
	basisRevision: number,
): NormalizedDashboardState {
	if (basisRevision < state.servicesRevision) return state;
	return {
		...upsertServiceRecord(state, status.service),
		allocationsByServiceId: {
			...state.allocationsByServiceId,
			[status.service.id]: {
				allocation: status.allocation,
				allocations: status.allocations,
				revision:
					state.statusRevisionByServiceId[status.service.id] !== undefined
						? state.statusRevisionByServiceId[status.service.id]
						: 0,
			},
		},
	};
}

export function removeService(
	state: NormalizedDashboardState,
	serviceId: string,
): NormalizedDashboardState {
	if (!state.servicesById[serviceId]) return state;
	const servicesById = { ...state.servicesById };
	delete servicesById[serviceId];
	const allocationsByServiceId = { ...state.allocationsByServiceId };
	delete allocationsByServiceId[serviceId];
	const statusRevisionByServiceId = { ...state.statusRevisionByServiceId };
	delete statusRevisionByServiceId[serviceId];
	return {
		...state,
		servicesById,
		serviceOrder: state.serviceOrder.filter((id) => id !== serviceId),
		allocationsByServiceId,
		statusRevisionByServiceId,
		selectedServiceId:
			state.selectedServiceId === serviceId ? null : state.selectedServiceId,
	};
}

function mergeRetainingCreated(
	services: Array<DashboardServiceRecord>,
	retained?: Array<DashboardServiceRecord>,
): Array<DashboardServiceRecord> {
	if (!retained || retained.length === 0) return services;
	const ids = new Set(services.map((service) => service.id));
	const missing = retained.filter((service) => !ids.has(service.id));
	return missing.length > 0 ? [...services, ...missing] : services;
}

function stripLoaderFields(
	loader: DashboardHomeState,
): NormalizedDashboardState["base"] {
	const {
		services: _services,
		servicesRevision: _servicesRevision,
		selectedServiceId: _selectedServiceId,
		...base
	} = loader;
	return base;
}
