import * as stylex from "@stylexjs/stylex";
import { useEffect, useId, useState } from "react";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import { PanelSection } from "#/components/ui/section";
import { useAutoQueuedPersist } from "#/hooks/use-auto-queued-persist";
import type { DashboardServiceRecord } from "#/lib/dashboard/core/types.server";
import { doScaleService } from "#/lib/dashboard/server-functions";
import { colors, space } from "#/styles/tokens.stylex";

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
	const volumeName = service.spec?.runtime?.volumeName?.trim();
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
			if (!updated.service)
				throw new Error("Service status is missing its service");
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
				{...stylex.props([
					styles.controls,
					pending && [fieldStyles.unappliedSurface, styles.pendingControls],
				])}
				data-unapplied={pending || undefined}
			>
				<label {...stylex.props(fieldStyles.label)} htmlFor={countId}>
					Current count
				</label>
				<TextInput
					id={countId}
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
				<p {...stylex.props(styles.volumeConstraint)}>
					Volume-backed services cannot run more than one replica
				</p>
			) : null}

			{inputError || error ? (
				<p {...stylex.props(noticeStyles.error)}>{inputError ?? error}</p>
			) : null}
			{saving ? (
				<p {...stylex.props(styles.volumeConstraint)}>Saving replica count…</p>
			) : null}
		</PanelSection>
	);
}

const styles = stylex.create({
	controls: { display: "flex", flexDirection: "column", gap: space.md },
	pendingControls: {
		borderStyle: "solid",
		borderWidth: "1px",
		padding: space.md,
	},
	volumeConstraint: {
		margin: "0rem",
		fontSize: "12px",
		lineHeight: "1.5",
		color: colors.muted,
	},
});
