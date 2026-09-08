import type {
	DashboardAllocationStatus,
	DashboardHomeState,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

/** Services list snapshot with its backend revision. */
export interface ServicesSnapshot {
	services: Array<DashboardServiceRecord>;
	revision: number;
}

/** Single service-status snapshot with its backend revision. */
export interface ServiceStatusSnapshot {
	status: DashboardServiceStatus;
	revision: number;
}

/** Allocation details for one service, stored separately from the record. */
export interface ServiceAllocations {
	allocation?: DashboardAllocationStatus;
	allocations?: Array<DashboardAllocationStatus>;
	revision: number;
}

/**
 * Normalized dashboard view state. Service records live exactly once in
 * `servicesById`; selection is a plain ID; allocation details live in
 * `allocationsByServiceId` keyed by the same ID. Nothing is duplicated, so
 * there is a single merge path per snapshot kind below.
 */
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

/** Ordered service array derived from the single by-ID source. */
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

/**
 * Reconstructed status for the selected service. The record comes from the
 * single by-ID source; only allocation details come from the status map, so
 * the two can never drift apart.
 */
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

/**
 * The single ordering gate for list snapshots (loader, list stream). A
 * snapshot at or below the applied revision is stale and ignored entirely,
 * which keeps a late loader from clearing fresher stream data. Causal
 * single-record writes bypass this gate via `upsertServiceRecord`.
 */
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

/**
 * Loader refresh. Static fields (project, environments, catalog) always come
 * from the loader; the service list still goes through the revision gate so a
 * stale loader rerender can never clear newer stream or mutation data. An
 * environment switch is a new scope: revisions are incomparable across
 * environments, so the incoming snapshot is accepted and selection resets to
 * the loader default.
 */
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
		// A loader with no environment is stale relative to optimistic
		// created state, not a scope switch: keep the current selection and
		// let the presence check below drop it only if the row is gone.
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
		// The loader hasn't caught up with created rows yet: union the
		// retained entries without moving the revision, so the next real
		// snapshot still applies.
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

/**
 * Causal single-record write (mutation responses, created-service cache
 * hydration). The caller just performed this write, so it always wins over
 * prior snapshots; future snapshots with a higher revision replace it.
 */
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

/**
 * The single ordering gate for status snapshots. Each service has its own
 * status revision; stale status data for one service never touches another.
 * The embedded service record is upserted through the same single path as
 * every other service write.
 */
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

/**
 * Mutation response with a basis revision captured when the request was
 * dispatched. If a newer list snapshot arrived while the request was in
 * flight, the response predates it and is ignored; the snapshot already
 * reflects the committed server state (backend indexes increment on every
 * mutation). Otherwise the causal write applies. This is the same revision
 * contract as snapshots, extended to request/response flows.
 */
export function applyMutationRecord(
	state: NormalizedDashboardState,
	service: DashboardServiceRecord,
	basisRevision: number,
): NormalizedDashboardState {
	if (basisRevision < state.servicesRevision) return state;
	return upsertServiceRecord(state, service);
}

/**
 * Mutation status response with a dispatch-time basis revision. The embedded
 * record goes through the same gate as every other service write;
 * allocation details apply alongside it.
 */
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

/**
 * Newly created services live in the above-remount cache until the backend
 * list includes them. Union them into snapshots so the optimistic row never
 * flickers; once the snapshot contains the ID the cache entry is redundant.
 */
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
