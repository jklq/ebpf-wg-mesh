import { useCallback, useEffect, useState } from "react";
import type { DashboardServiceSecret } from "#/lib/dashboard/core/types.server";
import {
	sealedSecretNameError,
	sealedSecretValue,
} from "#/lib/dashboard/sealed-secrets";
import {
	btnPrimary,
	btnSecondary,
	errorMsg,
	fieldInput,
	fieldLabel,
} from "#/lib/ui-classes";
import { DeleteDialog } from "./delete-dialog";
import {
	doDeleteServiceSecret,
	doSealServiceSecret,
	fetchServiceSecrets,
} from "./server-fns";
import { formatError } from "./service-utils";
import { PanelSection } from "./ui";

export function PanelSecrets({ serviceId }: { serviceId: string }) {
	const [secrets, setSecrets] = useState<DashboardServiceSecret[]>([]);
	const [name, setName] = useState("");
	const [value, setValue] = useState("");
	const [error, setError] = useState<string>();
	const [busy, setBusy] = useState(false);
	const [deleting, setDeleting] = useState<string>();
	const load = useCallback(async () => {
		setSecrets(await fetchServiceSecrets({ data: { serviceId } }));
	}, [serviceId]);
	useEffect(() => {
		void load().catch((cause) => setError(formatError(cause)));
	}, [load]);
	const save = async () => {
		const validation =
			sealedSecretNameError(name) ??
			sealedSecretValue.safeParse(value).error?.issues[0]?.message;
		if (validation) {
			setError(validation);
			return;
		}
		setBusy(true);
		setError(undefined);
		try {
			await doSealServiceSecret({ data: { serviceId, name, value } });
			setValue("");
			setName("");
			await load();
		} catch (cause) {
			setError(formatError(cause));
		} finally {
			setBusy(false);
		}
	};
	return (
		<PanelSection
			title="Sealed secrets"
			lede="Write-only values. Deploy to pin new versions. Remove a public variable before using the same name here. Duplicated environments do not copy secrets."
		>
			<div className="flex flex-col gap-3">
				{secrets.length === 0 && (
					<p className="text-sm text-muted">No sealed secrets yet.</p>
				)}
				{secrets.map((secret) => (
					<div
						key={secret.name}
						className="flex flex-wrap items-center gap-3 border border-line p-3 text-xs"
					>
						<span className="min-w-0 flex-1 break-all font-mono">
							{secret.name}
						</span>
						<span className="text-muted">
							v{secret.version} ·{" "}
							{secret.updatedAt
								? new Date(secret.updatedAt).toLocaleString()
								: "Unknown update time"}
						</span>
						<button
							type="button"
							className={btnSecondary}
							disabled={busy}
							onClick={() => {
								setName(secret.name);
								setValue("");
							}}
						>
							Update
						</button>
						<button
							type="button"
							className={btnSecondary}
							disabled={busy}
							onClick={() => {
								setError(undefined);
								setDeleting(secret.name);
							}}
						>
							Delete
						</button>
					</div>
				))}
				<form
					className="flex flex-col gap-3"
					onSubmit={(event) => {
						event.preventDefault();
						void save();
					}}
				>
					<label className={fieldLabel}>
						Secret name
						<input
							className={fieldInput}
							value={name}
							onChange={(e) => setName(e.target.value)}
							autoComplete="off"
							spellCheck={false}
						/>
					</label>
					<label className={fieldLabel}>
						Secret value
						<input
							className={fieldInput}
							type="password"
							value={value}
							onChange={(e) => setValue(e.target.value)}
							autoComplete="new-password"
						/>
					</label>
					<button type="submit" className={btnPrimary} disabled={busy}>
						{busy ? "Saving…" : "Seal secret"}
					</button>
				</form>
				{error && (
					<p role="alert" className={errorMsg}>
						{error}
					</p>
				)}
			</div>
			{deleting && (
				<DeleteDialog
					title="Delete secret"
					name={deleting}
					description="Future deployments will no longer include this secret."
					recovery="permanent"
					requireName={false}
					busy={busy}
					error={error}
					onCancel={() => setDeleting(undefined)}
					onConfirm={async () => {
						setBusy(true);
						setError(undefined);
						try {
							await doDeleteServiceSecret({
								data: { serviceId, name: deleting },
							});
							setDeleting(undefined);
							await load();
						} catch (cause) {
							setError(formatError(cause));
						} finally {
							setBusy(false);
						}
					}}
				/>
			)}
		</PanelSection>
	);
}
