import * as stylex from "@stylexjs/stylex";
import { Loader2, X } from "lucide-react";
import { Button } from "#/components/ui/button";
import { Dialog, dialogStyles } from "#/components/ui/dialog";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { noticeStyles } from "#/components/ui/notice";
import type { DomainController } from "#/features/dashboard/service-panel/domains/use-service-domains";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
const styles = stylex.create({
	domainForm: {
		display: "flex",
		flexDirection: "column",
		gap: space.lg,
		padding: space.xl,
	},
	dialogHeader: {
		display: "flex",
		alignItems: "center",
		justifyContent: "space-between",
	},
	dialogTitle: {
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		fontWeight: "600",
		color: colors.ink,
	},
	closeButton: { paddingInline: "0.375rem", paddingBlock: space.xs },
	portHint: {
		marginTop: "0.375rem",
		marginBottom: "0rem",
		fontSize: "11px",
		color: colors.muted,
	},
	generatedHostname: { fontFamily: fonts.mono },
	dnsInstructions: {
		backgroundColor: colors.surfaceRaised,
		paddingInline: space.md,
		paddingBlock: "0.625rem",
		fontFamily: fonts.mono,
		fontSize: "11px",
		lineHeight: "1.6",
		color: colors.muted,
	},
	dialogActions: { display: "flex", justifyContent: "flex-end", gap: space.sm },
	spinner: { animation: `${spin} 1s linear infinite` },
	editHostname: {
		marginBottom: space.md,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		color: colors.muted,
	},
	deleteDescription: { margin: "0rem", fontSize: "13px", color: colors.muted },
});
export function DomainDialogs({
	creation,
	editing,
	removal,
}: Pick<DomainController, "creation" | "editing" | "removal">) {
	const {
		domainFlow,
		platformBinding,
		targetPortId,
		targetPortRef,
		targetPort,
		setTargetPort,
		setError,
		recommendedPort,
		hostnameId,
		hostnameRef,
		hostname,
		setHostname,
		error,
		success,
		primaryDomainActionRef,
		generating,
		publishing,
		setDomainFlow,
		handlePublish,
		handleGenerate,
	} = creation;
	const {
		editingBinding,
		setEditingBinding,
		handleEditSave,
		editPortRef,
		editPort,
		setEditPort,
		setEditError,
		editError,
		editSaving,
	} = editing;
	const {
		deleteConfirm,
		setDeleteConfirm,
		deletingHostname,
		handleDelete,
		removeDomainRef,
	} = removal;
	return (
		<>
			{" "}
			{domainFlow && (
				<Dialog
					label={
						domainFlow === "generate" ? "Generate domain" : "Custom domain"
					}
					onClose={() => setDomainFlow(null)}
				>
					<form
						{...stylex.props([dialogStyles.card, styles.domainForm])}
						onSubmit={(event) => {
							event.preventDefault();
							if (domainFlow === "custom" && platformBinding) {
								if (!publishing && hostname.trim()) void handlePublish();
								return;
							}
							if (!generating) {
								void handleGenerate({ keepFlowOpen: domainFlow === "custom" });
							}
						}}
					>
						<div {...stylex.props(styles.dialogHeader)}>
							<span {...stylex.props(styles.dialogTitle)}>
								{domainFlow === "generate"
									? "Generate Domain"
									: "Custom Domain"}
							</span>
							<Button
								type="button"
								variant="ghost"
								styles={[styles.closeButton]}
								onClick={() => setDomainFlow(null)}
								aria-label="Close"
							>
								<X size={14} />
							</Button>
						</div>

						{(domainFlow === "generate" || !platformBinding) && (
							<div>
								<label
									{...stylex.props(fieldStyles.label)}
									htmlFor={targetPortId}
								>
									App port
								</label>
								<TextInput
									id={targetPortId}
									ref={targetPortRef}
									value={targetPort}
									onChange={(event) => {
										setTargetPort(event.target.value);
										setError(undefined);
									}}
									placeholder={String(
										platformBinding?.targetPort ?? recommendedPort,
									)}
									inputMode="numeric"
								/>
								<p {...stylex.props(styles.portHint)}>
									The port your app listens on inside the service.
								</p>
							</div>
						)}

						{domainFlow === "generate" && platformBinding && (
							<div
								{...stylex.props([
									noticeStyles.success,
									styles.generatedHostname,
								])}
							>
								{platformBinding.hostname}
							</div>
						)}

						{domainFlow === "custom" && platformBinding && (
							<>
								<div>
									<label
										{...stylex.props(fieldStyles.label)}
										htmlFor={hostnameId}
									>
										Custom hostname
									</label>
									<TextInput
										id={hostnameId}
										ref={hostnameRef}
										value={hostname}
										onChange={(event) => {
											setHostname(event.target.value);
											setError(undefined);
										}}
										placeholder="app.customer.com"
									/>
								</div>
								<div {...stylex.props(styles.dnsInstructions)}>
									Create this DNS record:
									<br />
									{hostname || "app.customer.com"} CNAME{" "}
									{platformBinding.hostname}
								</div>
							</>
						)}

						{error && <p {...stylex.props(noticeStyles.error)}>{error}</p>}
						{success && (
							<p {...stylex.props(noticeStyles.success)}>{success}</p>
						)}

						<div {...stylex.props(styles.dialogActions)}>
							<Button
								type="button"
								variant="secondary"
								onClick={() => setDomainFlow(null)}
							>
								Cancel
							</Button>
							{(domainFlow === "generate" || !platformBinding) && (
								<Button
									type="submit"
									ref={primaryDomainActionRef}
									variant="primary"
									disabled={generating}
								>
									{generating && (
										<Loader2 size={12} {...stylex.props(styles.spinner)} />
									)}
									{domainFlow === "custom"
										? "Generate & continue"
										: "Generate Domain"}
								</Button>
							)}
							{domainFlow === "custom" && platformBinding && (
								<Button
									type="submit"
									variant="primary"
									disabled={publishing || hostname.trim() === ""}
								>
									{publishing && (
										<Loader2 size={12} {...stylex.props(styles.spinner)} />
									)}
									Add domain
								</Button>
							)}
						</div>
					</form>
				</Dialog>
			)}
			{editingBinding && (
				<Dialog label="Edit domain" onClose={() => setEditingBinding(null)}>
					<form
						{...stylex.props([dialogStyles.card, styles.domainForm])}
						onSubmit={(event) => {
							event.preventDefault();
							void handleEditSave();
						}}
					>
						<div {...stylex.props(styles.dialogHeader)}>
							<span {...stylex.props(styles.dialogTitle)}>Edit domain</span>
							<Button
								type="button"
								variant="ghost"
								styles={[styles.closeButton]}
								onClick={() => setEditingBinding(null)}
							>
								<X size={14} />
							</Button>
						</div>
						<div>
							<p {...stylex.props(styles.editHostname)}>
								<span {...stylex.props(styles.generatedHostname)}>
									{editingBinding.hostname}
								</span>
							</p>
							<label {...stylex.props(fieldStyles.label)} htmlFor="edit-port">
								App port
							</label>
							<TextInput
								id="edit-port"
								ref={editPortRef}
								value={editPort}
								onChange={(e) => {
									setEditPort(e.target.value);
									setEditError(undefined);
								}}
								placeholder="8080"
								inputMode="numeric"
							/>
						</div>
						{editError && (
							<p {...stylex.props(noticeStyles.error)}>{editError}</p>
						)}
						<div {...stylex.props(styles.dialogActions)}>
							<Button
								type="button"
								variant="secondary"
								onClick={() => setEditingBinding(null)}
							>
								Cancel
							</Button>
							<Button
								type="submit"
								variant="primary"
								disabled={editSaving || editPort.trim() === ""}
							>
								{editSaving && (
									<Loader2 size={12} {...stylex.props(styles.spinner)} />
								)}
								Save
							</Button>
						</div>
					</form>
				</Dialog>
			)}
			{deleteConfirm && (
				<Dialog label="Remove domain" onClose={() => setDeleteConfirm(null)}>
					<form
						{...stylex.props([dialogStyles.card, styles.domainForm])}
						onSubmit={(event) => {
							event.preventDefault();
							if (!deletingHostname) void handleDelete(deleteConfirm);
						}}
					>
						<span {...stylex.props(styles.dialogTitle)}>Remove domain?</span>
						<p {...stylex.props(styles.deleteDescription)}>
							<span {...stylex.props(styles.generatedHostname)}>
								{deleteConfirm}
							</span>{" "}
							will stop routing traffic immediately.
						</p>
						<div {...stylex.props(styles.dialogActions)}>
							<Button
								type="button"
								variant="secondary"
								onClick={() => setDeleteConfirm(null)}
								disabled={deletingHostname === deleteConfirm}
							>
								Cancel
							</Button>
							<Button
								type="submit"
								ref={removeDomainRef}
								variant="danger"
								disabled={deletingHostname === deleteConfirm}
							>
								{deletingHostname === deleteConfirm && (
									<Loader2 size={12} {...stylex.props(styles.spinner)} />
								)}
								Remove
							</Button>
						</div>
					</form>
				</Dialog>
			)}
		</>
	);
}
