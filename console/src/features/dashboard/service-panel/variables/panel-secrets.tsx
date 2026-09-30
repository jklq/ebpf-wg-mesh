import * as stylex from "@stylexjs/stylex";
import { useCallback, useEffect, useId, useState } from "react";
import { DeleteResourceDialog } from "#/components/delete-resource-dialog";
import { Button } from "#/components/ui/button";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import { PanelSection } from "#/components/ui/section";
import type { DashboardServiceSecret } from "#/lib/dashboard/core/types.server";
import {
	sealedSecretNameError,
	sealedSecretValue,
} from "#/lib/dashboard/sealed-secrets";
import {
	doDeleteServiceSecret,
	doSealServiceSecret,
	fetchServiceSecrets,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export function PanelSecrets({ serviceId }: { serviceId: string }) {
	const formId = useId();
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
			<div {...stylex.props(styles.secrets)}>
				{secrets.length === 0 && (
					<p {...stylex.props(styles.emptyState)}>No sealed secrets yet.</p>
				)}
				{secrets.map((secret) => (
					<div key={secret.name} {...stylex.props(styles.secretRow)}>
						<span {...stylex.props(styles.secretName)}>{secret.name}</span>
						<span {...stylex.props(styles.secretMetadata)}>
							v{secret.version} ·{" "}
							{secret.updatedAt
								? new Date(secret.updatedAt).toLocaleString()
								: "Unknown update time"}
						</span>
						<Button
							type="button"
							variant="secondary"
							disabled={busy}
							onClick={() => {
								setName(secret.name);
								setValue("");
							}}
						>
							Update
						</Button>
						<Button
							type="button"
							variant="secondary"
							disabled={busy}
							onClick={() => {
								setError(undefined);
								setDeleting(secret.name);
							}}
						>
							Delete
						</Button>
					</div>
				))}
				<form
					{...stylex.props(styles.secrets)}
					onSubmit={(event) => {
						event.preventDefault();
						void save();
					}}
				>
					<label
						htmlFor={`${formId}-name`}
						{...stylex.props(fieldStyles.label)}
					>
						Secret name
						<TextInput
							id={`${formId}-name`}
							value={name}
							onChange={(e) => setName(e.target.value)}
							autoComplete="off"
							spellCheck={false}
						/>
					</label>
					<label
						htmlFor={`${formId}-value`}
						{...stylex.props(fieldStyles.label)}
					>
						Secret value
						<TextInput
							id={`${formId}-value`}
							type="password"
							value={value}
							onChange={(e) => setValue(e.target.value)}
							autoComplete="new-password"
						/>
					</label>
					<Button type="submit" variant="primary" disabled={busy}>
						{busy ? "Saving…" : "Seal secret"}
					</Button>
				</form>
				{error && (
					<p role="alert" {...stylex.props(noticeStyles.error)}>
						{error}
					</p>
				)}
			</div>
			{deleting && (
				<DeleteResourceDialog
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

const styles = stylex.create({
	secrets: { display: "flex", flexDirection: "column", gap: space.md },
	emptyState: {
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		color: colors.muted,
	},
	secretRow: {
		display: "flex",
		flexWrap: "wrap",
		alignItems: "center",
		gap: space.md,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		padding: space.md,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
	},
	secretName: {
		minWidth: "0rem",
		flex: "1",
		wordBreak: "break-all",
		fontFamily: fonts.mono,
	},
	secretMetadata: { color: colors.muted },
});
