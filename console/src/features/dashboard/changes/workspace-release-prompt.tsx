import * as stylex from "@stylexjs/stylex";
import { Loader2, UploadCloud } from "lucide-react";
import { Button } from "#/components/ui/button";
import { colors, fonts, sizes, space } from "#/styles/tokens.stylex";
import {
	deployActionLabel,
	dirtyPromptDetail,
	dirtyPromptTitle,
} from "./dashboard-unapplied";
import type { useEnvironmentRelease } from "./use-environment-release";

type ReleasePromptModel = Pick<
	ReturnType<typeof useEnvironmentRelease>,
	| "changeSignature"
	| "deployError"
	| "applyingChangeCount"
	| "deployingChanges"
	| "deployableUnappliedChanges"
	| "hasPendingSpecWrites"
	| "totalUnappliedChanges"
	| "setShowChangeDetails"
	| "handleDeployChanges"
>;
export function WorkspaceReleasePrompt({
	release,
	panelOpen,
}: {
	release: ReleasePromptModel;
	panelOpen: boolean;
}) {
	return (
		<div
			data-workspace-prompt=""
			{...stylex.props([
				styles.promptPosition,
				styles.promptOffset(panelOpen ? sizes.sidePanel : 0),
				panelOpen && styles.promptHiddenOnMobile,
			])}
		>
			<div
				key={release.changeSignature || "deploy-prompt"}
				{...stylex.props([
					styles.prompt,
					Boolean(release.deployError) && styles.promptError,
					!release.deployError &&
						release.applyingChangeCount > 0 &&
						styles.promptApplying,
					Boolean(release.changeSignature) && styles.promptNudge,
				])}
			>
				<div {...stylex.props(styles.promptCopy)}>
					<span {...stylex.props(styles.promptTitle)}>
						{dirtyPromptTitle({
							applying: release.applyingChangeCount,
							deployError: release.deployError,
							deploying: release.deployingChanges,
						})}
					</span>
					<span {...stylex.props(styles.promptDetail)}>
						{dirtyPromptDetail({
							applying: release.applyingChangeCount,
							deployable: release.deployableUnappliedChanges,
							saving: release.hasPendingSpecWrites,
							total: release.totalUnappliedChanges,
						})}
					</span>
					{release.deployError && (
						<span {...stylex.props(styles.promptErrorText)}>
							{release.deployError}
						</span>
					)}
				</div>
				<Button
					type="button"
					variant="secondary"
					onClick={() => release.setShowChangeDetails(true)}
				>
					Details
				</Button>
				<Button
					type="button"
					variant="primary"
					onClick={release.handleDeployChanges}
					disabled={
						release.deployingChanges ||
						(!release.hasPendingSpecWrites &&
							release.deployableUnappliedChanges === 0)
					}
				>
					{release.deployingChanges ? (
						<Loader2 size={13} {...stylex.props(styles.spinner)} />
					) : (
						<UploadCloud size={13} />
					)}
					{deployActionLabel({ deploying: release.deployingChanges })}
				</Button>
			</div>
		</div>
	);
}
const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
const dirtyBannerNudge = stylex.keyframes({
	"0%": {
		transform: "translateY(0) scale(1)",
		boxShadow: `inset 3px 0 0 ${colors.accent}, 0 16px 40px rgba(0, 0, 0, 0.4)`,
	},
	"28%": {
		transform: "translateY(-7px) scale(1.035)",
		boxShadow: `inset 3px 0 0 ${colors.accent}, 0 22px 48px rgba(0, 0, 0, 0.5), 0 0 0 1px rgba(212, 168, 88, 0.35)`,
	},
	"58%": { transform: "translateY(1px) scale(0.992)" },
	"100%": { transform: "translateY(0) scale(1)" },
});
const styles = stylex.create({
	promptOffset: (value: string | number) => ({ right: value }),
	spinner: { animation: `${spin} 1s linear infinite` },
	promptPosition: {
		pointerEvents: "none",
		position: "absolute",
		top: { default: "18px", "@media (width < 40rem)": "0.75rem" },
		right: "0rem",
		left: "0rem",
		zIndex: "35",
		display: "flex",
		justifyContent: "center",
		paddingInline: space.xl,
	},
	promptHiddenOnMobile: {
		display: { default: "flex", "@media (width < 900px)": "none" },
	},
	prompt: {
		pointerEvents: "auto",
		display: "flex",
		width: "fit-content",
		minWidth: "min(520px, 100%)",
		maxWidth: "100%",
		alignItems: { default: "center", "@media (width < 40rem)": "stretch" },
		gap: "0.875rem",
		overflow: "hidden",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: colors.surface,
		backgroundImage:
			"linear-gradient(180deg,rgba(255,255,255,0.04),transparent 40%)",
		paddingBlock: "11px",
		paddingRight: space.md,
		paddingLeft: space.lg,
		boxShadow: `inset 3px 0 0 ${colors.accent}, 0 16px 40px rgba(0,0,0,0.4)`,
		flexDirection: { default: null, "@media (width < 40rem)": "column" },
	},
	promptError: {
		borderColor: `color-mix(in oklab, ${colors.failed} 50%, transparent)`,
		boxShadow: `inset 3px 0 0 ${colors.failed}, 0 16px 40px rgba(0,0,0,0.4)`,
	},
	promptApplying: {
		borderColor: `color-mix(in oklab, ${colors.building} 50%, transparent)`,
		boxShadow: `inset 3px 0 0 ${colors.building}, 0 16px 40px rgba(0,0,0,0.4)`,
	},
	promptNudge: {
		animation: `${dirtyBannerNudge} 0.52s cubic-bezier(0.18, 1.35, 0.28, 1)`,
	},
	promptCopy: {
		display: "flex",
		minWidth: "0rem",
		flex: "1",
		alignItems: "baseline",
		gap: space.sm,
		overflow: "hidden",
		flexWrap: { default: null, "@media (width < 40rem)": "wrap" },
	},
	promptTitle: {
		flexShrink: "0",
		fontFamily: fonts.display,
		fontSize: "18px",
		fontWeight: "500",
		letterSpacing: "-0.02em",
		color: colors.ink,
	},
	promptDetail: {
		minWidth: "0rem",
		overflow: "hidden",
		fontFamily: fonts.mono,
		fontSize: "11px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.muted,
	},
	promptErrorText: {
		minWidth: "0rem",
		overflow: "hidden",
		fontFamily: fonts.mono,
		fontSize: "11px",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		color: colors.failed,
	},
});
