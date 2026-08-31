import {
	type Dispatch,
	type SetStateAction,
	useEffect,
	useRef,
	useState,
} from "react";

import { formatError } from "./service-utils";

export function useAutoQueuedPersist<T>({
	serviceId,
	incoming,
	incomingEpoch,
	persist,
	enabled = true,
	delay = 120,
}: {
	serviceId: string;
	incoming: T;
	incomingEpoch?: string | number;
	persist: (draft: T) => Promise<void>;
	enabled?: boolean | ((draft: T) => boolean);
	delay?: number;
}): {
	draft: T;
	setDraft: Dispatch<SetStateAction<T>>;
	error?: string;
	saving: boolean;
} {
	const persistRef = useRef(persist);
	persistRef.current = persist;
	const incomingRef = useRef(incoming);
	incomingRef.current = incoming;
	const incomingEpochRef = useRef(incomingEpoch);
	incomingEpochRef.current = incomingEpoch;
	const enabledRef = useRef(enabled);
	enabledRef.current = enabled;
	const [draft, setDraft] = useState(incoming);
	const draftRef = useRef(draft);
	draftRef.current = draft;
	const lastAckedRef = useRef(stableKey(incoming));
	const submittedKeysRef = useRef(new Set<string>());
	const epochRef = useRef(incomingEpoch);
	const persistGenerationRef = useRef(0);
	const [error, setError] = useState<string>();
	const [saving, setSaving] = useState(false);
	const draftKey = stableKey(draft);
	const incomingKey = stableKey(incoming);
	// Only a boolean `enabled` belongs in effect deps. Callers pass inline
	// predicates, and those new function identities must not retrigger persist.
	const enabledFlag = typeof enabled === "boolean" ? enabled : true;

	useEffect(() => {
		if (!serviceId) {
			return;
		}
		const next = incomingRef.current;
		setDraft(next);
		lastAckedRef.current = stableKey(next);
		submittedKeysRef.current.clear();
		persistGenerationRef.current += 1;
		epochRef.current = incomingEpochRef.current;
		setError(undefined);
	}, [serviceId]);

	useEffect(() => {
		if (incomingEpoch === epochRef.current) {
			return;
		}
		epochRef.current = incomingEpoch;
		if (submittedKeysRef.current.has(incomingKey)) {
			submittedKeysRef.current.delete(incomingKey);
			lastAckedRef.current = incomingKey;
			return;
		}
		// A subscription refresh must never replace an edit which is waiting for
		// its debounce or is queued behind another editor for this service.
		if (
			draftKey !== lastAckedRef.current ||
			submittedKeysRef.current.size > 0
		) {
			return;
		}
		if (incomingKey === lastAckedRef.current || incomingKey === draftKey) {
			lastAckedRef.current = incomingKey;
			return;
		}
		setDraft(incomingRef.current);
		lastAckedRef.current = incomingKey;
	}, [draftKey, incomingEpoch, incomingKey]);

	useEffect(() => {
		if (
			!enabledFlag ||
			!isPersistEnabled(enabledRef.current, draftRef.current)
		) {
			persistGenerationRef.current += 1;
			setSaving(false);
			return;
		}
		if (
			draftKey === lastAckedRef.current &&
			submittedKeysRef.current.size === 0
		) {
			persistGenerationRef.current += 1;
			setSaving(false);
			return;
		}
		const generation = persistGenerationRef.current + 1;
		persistGenerationRef.current = generation;
		setSaving(true);
		const timer = window.setTimeout(() => {
			const next = draftRef.current;
			const nextKey = stableKey(next);
			if (
				(nextKey === lastAckedRef.current &&
					submittedKeysRef.current.size === 0) ||
				!isPersistEnabled(enabledRef.current, next)
			) {
				if (generation === persistGenerationRef.current) setSaving(false);
				return;
			}
			submittedKeysRef.current.add(nextKey);
			setError(undefined);
			void enqueueServicePersist(serviceId, () => persistRef.current(next))
				.then(() => {
					lastAckedRef.current = nextKey;
				})
				.catch((cause) => {
					submittedKeysRef.current.delete(nextKey);
					if (generation === persistGenerationRef.current) {
						setError(formatError(cause));
					}
				})
				.finally(() => {
					if (generation === persistGenerationRef.current) {
						setSaving(false);
					}
				});
		}, delay);
		return () => window.clearTimeout(timer);
	}, [delay, draftKey, enabledFlag, serviceId]);

	return { draft, setDraft, error, saving };
}

function isPersistEnabled<T>(
	enabled: boolean | ((draft: T) => boolean),
	draft: T,
): boolean {
	return typeof enabled === "function" ? enabled(draft) : enabled;
}

function stableKey(value: unknown): string {
	return JSON.stringify(value);
}

const servicePersistTails = new Map<string, Promise<unknown>>();

export function enqueueServicePersist<T>(
	serviceId: string,
	persist: () => Promise<T>,
): Promise<T> {
	const previous = servicePersistTails.get(serviceId) ?? Promise.resolve();
	const current = previous.catch(() => undefined).then(persist);
	servicePersistTails.set(serviceId, current);
	const cleanup = () => {
		if (servicePersistTails.get(serviceId) === current) {
			servicePersistTails.delete(serviceId);
		}
	};
	void current.then(cleanup, cleanup);
	return current;
}
