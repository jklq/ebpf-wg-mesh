import { useEffect, useId, useState } from "react";
import { cn } from "#/lib/cn";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import {
	errorMsg,
	fieldInput,
	fieldLabel,
	unappliedSurface,
} from "#/lib/ui-classes";

import { doScaleService } from "./server-fns";
import { PanelSection } from "./ui";
import { useAutoQueuedPersist } from "./use-auto-queued-persist";

const MIN_REPLICAS = 1;
const MAX_REPLICAS = 64;

export function ReplicaScaleControls({
	service,
	onQueued,
	onSavingChange,
}: {
	service: DashboardServiceRecord;
	onQueued: (service: DashboardServiceRecord) => void;
	onSavingChange?: (saving: boolean) => void;
}) {
	const volumeName = service.spec?.runtime.volumeName?.trim();
	const live = service.desiredReplicaCount ?? 1;
	const queued = service.spec?.desiredReplicaCount ?? live;
	const pending = queued !== live;
	const countId = useId();
	const [draft, setDraft] = useState(String(queued));
	const [inputError, setInputError] = useState<string>();
	const {
		setDraft: setDesiredCount,
		error,
		saving,
	} = useAutoQueuedPersist({
		serviceId: service.id,
		incoming: queued,
		incomingEpoch: service.specRevision,
		enabled: (next) =>
			Number.isInteger(next) &&
			next >= MIN_REPLICAS &&
			next <= MAX_REPLICAS &&
			!(next > 1 && volumeName),
		persist: async (next) => {
			const updated = await doScaleService({
				data: { serviceId: service.id, desiredReplicaCount: next },
			});
			onQueued(updated.service);
		},
	});

	useEffect(() => {
		setDraft(String(queued));
	}, [queued]);

	useEffect(() => {
		onSavingChange?.(saving);
		return () => onSavingChange?.(false);
	}, [onSavingChange, saving]);

	const queueCount = (next: number) => {
		if (next < MIN_REPLICAS || next > MAX_REPLICAS) return;
		if (next > 1 && volumeName) {
			setInputError("Volume-backed services cannot run more than one replica");
			setDraft(String(queued));
			return;
		}
		setInputError(undefined);
		setDraft(String(next));
		setDesiredCount(next);
	};

	const parsedDraft = Number(draft.trim());
	const draftIsValid =
		Number.isInteger(parsedDraft) &&
		parsedDraft >= MIN_REPLICAS &&
		parsedDraft <= MAX_REPLICAS;

	return (
		<PanelSection title="Replicas">
			<div
				className={cn(
					"flex flex-col gap-3",
					pending && cn(unappliedSurface, "border p-3"),
				)}
				data-unapplied={pending || undefined}
			>
				<label className={fieldLabel} htmlFor={countId}>
					Current count
				</label>
				<input
					id={countId}
					className={fieldInput}
					value={draft}
					onChange={(event) => {
						const value = event.target.value;
						setDraft(value);
						setInputError(undefined);
						const parsed = Number(value.trim());
						if (
							!Number.isInteger(parsed) ||
							parsed < MIN_REPLICAS ||
							parsed > MAX_REPLICAS
						) {
							return;
						}
						queueCount(parsed);
					}}
					onBlur={() => {
						if (!draftIsValid) {
							setInputError(
								`Enter a whole number from ${MIN_REPLICAS} to ${MAX_REPLICAS}`,
							);
							setDraft(String(queued));
							return;
						}
						queueCount(parsedDraft);
					}}
					inputMode="numeric"
					autoComplete="off"
				/>
			</div>

			{volumeName ? (
				<p className="m-0 text-[12px] leading-normal text-muted">
					Volume-backed services cannot run more than one replica
				</p>
			) : null}

			{inputError || error ? (
				<p className={errorMsg}>{inputError ?? error}</p>
			) : null}
			{saving ? (
				<p className="m-0 text-[12px] leading-normal text-muted">
					Saving replica count…
				</p>
			) : null}
		</PanelSection>
	);
}
