import { Loader2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { cn } from "#/lib/cn";
import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import {
	btnPrimary,
	errorMsg,
	fieldInput,
	iconBtn,
	modalCard,
} from "#/lib/ui-classes";

import { doCreateEnvironment, doDuplicateEnvironment } from "./server-fns";
import { formatError } from "./service-utils";
import { ModalOverlay } from "./ui";

const envOptionCard =
	"flex cursor-pointer flex-col gap-2 rounded-sm border p-3 transition-[border-color,background-color] duration-100 ease-out";
const envOptionIdle = "border-line bg-surface-raised hover:border-line-bright";
const envOptionSelected = "border-accent bg-accent-dim";

export function EnvironmentDialog({
	state,
	onClose,
	onCreated,
}: {
	state: DashboardHomeState;
	onClose: () => void;
	onCreated: (environmentId: string) => void;
}) {
	const [name, setName] = useState("");
	const [mode, setMode] = useState<"duplicate" | "empty">(
		state.environments.length > 0 ? "duplicate" : "empty",
	);
	const [sourceId, setSourceId] = useState(
		state.environment?.id ?? state.environments[0]?.id ?? "",
	);
	const [copyVariables, setCopyVariables] = useState(true);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string>();
	const nameInputRef = useRef<HTMLInputElement>(null);
	const project = state.project;
	const canDuplicate = state.environments.length > 0;

	useEffect(() => {
		nameInputRef.current?.focus();
	}, []);

	const submit = async () => {
		if (!project || busy || !name.trim()) return;
		setBusy(true);
		setError(undefined);
		try {
			const created =
				mode === "duplicate" && sourceId
					? await doDuplicateEnvironment({
							data: {
								sourceEnvironmentId: sourceId,
								name: name.trim(),
								copyVariables,
							},
						})
					: await doCreateEnvironment({
							data: { projectId: project.id, name: name.trim() },
						});
			setBusy(false);
			onCreated(created.id);
		} catch (cause) {
			setError(formatError(cause));
			setBusy(false);
		}
	};

	return (
		<ModalOverlay
			ariaLabel="New environment"
			onClose={() => {
				if (!busy) onClose();
			}}
		>
			<form
				className={cn(
					modalCard,
					"flex max-w-[460px] flex-col gap-3.5 p-[18px]",
				)}
				onSubmit={(event) => {
					event.preventDefault();
					void submit();
				}}
			>
				<div className="flex items-center justify-between gap-3">
					<h2 className="m-0 font-condensed text-[22px] tracking-[0.02em] text-ink">
						New Environment
					</h2>
					<button
						type="button"
						className={iconBtn}
						aria-label="Close"
						onClick={onClose}
						disabled={busy}
					>
						<X size={15} />
					</button>
				</div>

				<p className="-mt-1.5 mb-0 text-xs leading-normal text-muted">
					All the changes will be isolated from other environments — deploy,
					edit, and break things without touching production.
				</p>

				<input
					ref={nameInputRef}
					className={cn(fieldInput, "font-sans text-sm")}
					value={name}
					onChange={(event) => setName(event.target.value)}
					placeholder="staging"
					aria-label="Environment name"
					autoComplete="off"
					spellCheck={false}
					disabled={busy}
				/>

				{canDuplicate && (
					<label
						className={cn(
							envOptionCard,
							mode === "duplicate" ? envOptionSelected : envOptionIdle,
						)}
					>
						<span className="flex items-center gap-2">
							<input
								type="radio"
								name="environment-mode"
								className="size-3.5 accent-accent"
								checked={mode === "duplicate"}
								onChange={() => setMode("duplicate")}
								disabled={busy}
							/>
							<span className="text-[13px] font-semibold text-ink">
								Duplicate Environment
							</span>
						</span>
						<select
							className={cn(fieldInput, "cursor-pointer font-sans")}
							value={sourceId}
							onChange={(event) => setSourceId(event.target.value)}
							onClick={(event) => event.stopPropagation()}
							aria-label="Source environment"
							disabled={busy || mode !== "duplicate"}
						>
							{state.environments.map((entry) => (
								<option key={entry.id} value={entry.id}>
									{entry.name}
								</option>
							))}
						</select>
						<span className="text-xs leading-normal text-muted">
							Copy all the services and configuration from an existing
							environment.
						</span>
						{mode === "duplicate" && (
							<span className="flex flex-wrap items-center gap-1.5 text-xs text-muted">
								<input
									type="checkbox"
									className="accent-accent"
									checked={copyVariables}
									onChange={(event) => setCopyVariables(event.target.checked)}
									disabled={busy}
								/>
								Copy variables
								{copyVariables && (
									<span className="basis-full text-[11px] text-building">
										Copied values may contain production credentials.
									</span>
								)}
							</span>
						)}
					</label>
				)}

				<label
					className={cn(
						envOptionCard,
						mode === "empty" ? envOptionSelected : envOptionIdle,
					)}
				>
					<span className="flex items-center gap-2">
						<input
							type="radio"
							name="environment-mode"
							className="size-3.5 accent-accent"
							checked={mode === "empty"}
							onChange={() => setMode("empty")}
							disabled={busy}
						/>
						<span className="text-[13px] font-semibold text-ink">
							Empty Environment
						</span>
					</span>
					<span className="text-xs leading-normal text-muted">
						An empty environment with no services or variables included.
					</span>
				</label>

				{error && <p className={errorMsg}>{error}</p>}

				<button
					type="submit"
					className={cn(btnPrimary, "w-full justify-center !py-2.5")}
					disabled={busy || !name.trim() || !project}
				>
					{busy && <Loader2 size={13} className="animate-spin" />}
					{busy ? "Creating…" : "Create Environment"}
				</button>
			</form>
		</ModalOverlay>
	);
}
