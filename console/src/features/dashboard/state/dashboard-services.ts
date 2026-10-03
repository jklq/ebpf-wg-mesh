import type {
	DashboardAllocationStatus,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import { compareIntegers } from "#/lib/platform-json";

export interface ServicesSnapshot {
	services: Array<DashboardServiceRecord>;
	/** Volumes ride the environment snapshot; absent means unchanged. */
	volumes?: Array<DashboardVolume>;
	revision: string;
}

export interface ServiceStatusSnapshot {
	status: DashboardServiceStatus;
	revision: string;
}

export interface ServiceAllocations {
	allocation?: DashboardAllocationStatus;
	allocations?: Array<DashboardAllocationStatus>;
	revision: string;
}

export interface NormalizedDashboardState {
	base: Omit<
		DashboardHomeState,
		"services" | "servicesRevision" | "selectedServiceId"
	>;
	servicesById: Record<string, DashboardServiceRecord>;
	serviceOrder: Array<string>;
	servicesRevision: string;
	selectedServiceId: string | null;
	allocationsByServiceId: Record<string, ServiceAllocations>;
	statusRevisionByServiceId: Record<string, string>;
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
		allocations: details.allocations ?? [],
		index: details.revision,
		notModified: false,
	};
}

export function applyServicesSnapshot(
	state: NormalizedDashboardState,
	snapshot: ServicesSnapshot,
	retained?: Array<DashboardServiceRecord>,
	force?: boolean,
): NormalizedDashboardState {
	if (!force && compareIntegers(snapshot.revision, state.servicesRevision) <= 0)
		return state;
	const merged = mergeRetainingCreated(snapshot.services, retained);
	const byId: Record<string, DashboardServiceRecord> = {};
	const order: Array<string> = [];
	for (const service of merged) {
		if (!byId[service.id]) order.push(service.id);
		const current = state.servicesById[service.id];
		byId[service.id] =
			!force &&
			current &&
			(serviceVersionOrder(service, current) < 0 ||
				compareIntegers(
					snapshot.revision,
					state.statusRevisionByServiceId[service.id] ?? "0",
				) < 0)
				? current
				: service;
	}
	const selectedKept =
		state.selectedServiceId && byId[state.selectedServiceId]
			? state.selectedServiceId
			: null;
	return {
		...state,
		base: snapshot.volumes
			? { ...state.base, volumes: snapshot.volumes }
			: state.base,
		servicesById: byId,
		serviceOrder: order,
		servicesRevision: snapshot.revision,
		selectedServiceId: selectedKept,
	};
}

/** Applies a volume returned by a mutation ahead of the next snapshot. */
export function upsertVolume(
	state: NormalizedDashboardState,
	volume: DashboardVolume,
): NormalizedDashboardState {
	const volumes = state.base.volumes.some((entry) => entry.id === volume.id)
		? state.base.volumes.map((entry) =>
				entry.id === volume.id ? volume : entry,
			)
		: [...state.base.volumes, volume];
	return { ...state, base: { ...state.base, volumes } };
}

export function removeVolume(
	state: NormalizedDashboardState,
	volumeId: string,
): NormalizedDashboardState {
	return {
		...state,
		base: {
			...state.base,
			volumes: state.base.volumes.filter((entry) => entry.id !== volumeId),
		},
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
			allocationsByServiceId: {},
			statusRevisionByServiceId: {},
			selectedServiceId: loader.environment
				? loader.selectedServiceId
				: state.selectedServiceId,
		};
		return applyServicesSnapshot(
			reset,
			{
				services: loader.services,
				volumes: loader.volumes,
				revision: loader.servicesRevision,
			},
			retained,
			true,
		);
	}
	if (compareIntegers(loader.servicesRevision, state.servicesRevision) <= 0) {
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
		{
			services: loader.services,
			volumes: loader.volumes,
			revision: loader.servicesRevision,
		},
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
	const service = snapshot.status.service;
	if (!service) throw new Error("Service status is missing its service");
	const serviceId = service.id;
	if (
		compareIntegers(
			snapshot.revision,
			state.statusRevisionByServiceId[serviceId] ?? "-1",
		) <= 0
	) {
		return state;
	}
	const current = state.servicesById[serviceId];
	const record =
		current && serviceVersionOrder(service, current) < 0
			? current
			: withCanvasMetadata(service, current);
	return {
		...upsertServiceRecord(state, record),
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
	basisRevision: string,
): NormalizedDashboardState {
	const current = state.servicesById[service.id];
	const order = current ? serviceVersionOrder(service, current) : 0;
	if (
		order < 0 ||
		(order === 0 && compareIntegers(basisRevision, state.servicesRevision) < 0)
	)
		return state;
	return upsertServiceRecord(state, service);
}

export function applyMutationStatus(
	state: NormalizedDashboardState,
	status: DashboardServiceStatus,
	basisRevision: string,
): NormalizedDashboardState {
	const service = status.service;
	if (!service) throw new Error("Service status is missing its service");
	const next = applyMutationRecord(
		state,
		withCanvasMetadata(service, state.servicesById[service.id]),
		basisRevision,
	);
	if (next === state) return state;
	const statusRevision = state.statusRevisionByServiceId[service.id] ?? "0";
	const responseRevision =
		compareIntegers(status.index, statusRevision) > 0
			? status.index
			: statusRevision;
	return {
		...next,
		allocationsByServiceId: {
			...state.allocationsByServiceId,
			[service.id]: {
				allocation: status.allocation,
				allocations: status.allocations,
				revision: responseRevision,
			},
		},
		statusRevisionByServiceId: {
			...state.statusRevisionByServiceId,
			[service.id]: responseRevision,
		},
	};
}

// Event indices belong to the whole environment. A reply can still advance one
// service after another service has advanced that index.
function serviceVersionOrder(
	incoming: DashboardServiceRecord,
	current: DashboardServiceRecord,
): number {
	const spec = compareIntegers(incoming.specRevision, current.specRevision);
	const rollout = compareIntegers(
		incoming.rolloutGeneration,
		current.rolloutGeneration,
	);
	if (spec < 0 || rollout < 0) return -1;
	if (spec > 0 || rollout > 0) return 1;
	const updated =
		Date.parse(incoming.updatedAt ?? "") - Date.parse(current.updatedAt ?? "");
	return Number.isFinite(updated) ? Math.sign(updated) : 0;
}

function withCanvasMetadata(
	service: DashboardServiceRecord,
	current?: DashboardServiceRecord,
): DashboardServiceRecord {
	return {
		...service,
		...(current?.projectId ? { projectId: current.projectId } : {}),
		...(current?.layoutPosition
			? { layoutPosition: current.layoutPosition }
			: {}),
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
