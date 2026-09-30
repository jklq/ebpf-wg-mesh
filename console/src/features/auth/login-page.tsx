import * as stylex from "@stylexjs/stylex";
import { Zap } from "lucide-react";
import { noticeStyles } from "#/components/ui/notice";
import type {
	DashboardHomeState,
	DevLoginIdentity,
} from "#/lib/dashboard/core/types.server";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	page: {
		display: "flex",
		minHeight: "100dvh",
		alignItems: "center",
		justifyContent: "center",
		overflow: "auto",
		backgroundColor: colors.canvas,
		padding: space.xl,
	},
	content: { width: "100%", maxWidth: "400px" },
	brand: {
		marginBottom: space.xxl,
		display: "flex",
		alignItems: "center",
		gap: space.sm,
	},
	brandIcon: { color: colors.accent },
	brandName: {
		fontFamily: fonts.condensed,
		fontSize: "1.125rem",
		lineHeight: "calc(1.75 / 1.125)",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.14em",
		color: colors.ink,
	},
	card: {
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.lineBright,
		backgroundColor: colors.surface,
		padding: "1.75rem",
	},
	title: {
		margin: "0rem",
		marginBottom: "0.375rem",
		fontFamily: fonts.condensed,
		fontSize: "22px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.04em",
		color: colors.ink,
	},
	description: {
		marginTop: "0rem",
		marginBottom: space.xl,
		fontSize: "13px",
		lineHeight: "1.5",
		color: colors.muted,
	},
	errorMessage: { marginBottom: "1.25rem" },
	loginMethods: { display: "flex", flexDirection: "column", gap: "0.625rem" },
	githubLink: {
		display: "flex",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "space-between",
		borderRadius: "0",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.accent },
		},
		backgroundColor: {
			default: colors.surfaceRaised,
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
		paddingInline: space.lg,
		paddingBlock: "0.875rem",
		textDecorationLine: "none",
		transitionProperty: "border-color,background-color",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
	},
	methodTitle: {
		margin: "0rem",
		fontSize: "0.875rem",
		lineHeight: "calc(1.25 / 0.875)",
		fontWeight: "600",
		letterSpacing: "0.04em",
		color: colors.ink,
	},
	methodDescription: {
		marginTop: "0.125rem",
		marginBottom: "0rem",
		fontSize: "11px",
		color: colors.muted,
	},
	methodArrow: {
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		fontWeight: "600",
		color: colors.accent,
	},
	devLogins: {
		overflow: "hidden",
		borderRadius: "0",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
	},
	devLoginsHeader: {
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.lg,
		paddingBlock: space.sm,
	},
	devLoginsLabel: {
		fontFamily: fonts.condensed,
		fontSize: "10px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.muted,
	},
	devLoginLink: {
		display: "flex",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "space-between",
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.lg,
		paddingBlock: space.md,
		textDecorationLine: "none",
		transitionProperty:
			"color, background-color, border-color, outline-color, text-decoration-color, fill, stroke",
		transitionTimingFunction: "cubic-bezier(0.4, 0, 0.2, 1)",
		transitionDuration: motion.fast,
		backgroundColor: {
			default: null,
			":hover": { default: null, "@media (hover: hover)": colors.surfaceHover },
		},
	},
	devEmail: {
		margin: "0rem",
		fontSize: "13px",
		fontWeight: "600",
		color: colors.ink,
	},
	devUserId: {
		marginTop: "1px",
		marginBottom: "0rem",
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.muted,
	},
	unconfiguredNotice: {
		borderRadius: "0",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		padding: space.lg,
		fontSize: "0.75rem",
		lineHeight: "1.625",
		color: colors.muted,
	},
	configVariable: {
		borderRadius: "0",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.canvas,
		paddingInline: "0.375rem",
		paddingBlock: "1px",
		fontFamily: fonts.mono,
		fontSize: "11px",
	},
});
export interface LoginRouteState {
	session: DashboardHomeState | null;
	devUsers: Array<DevLoginIdentity>;
	githubLoginEnabled: boolean;
	publicBaseURL: string;
}
export function LoginPageView({
	state,
	redirectTo,
	error,
	errorDetail,
}: {
	state: LoginRouteState;
	redirectTo?: string;
	error?: string;
	errorDetail?: string;
}) {
	return (
		<main {...stylex.props(styles.page)}>
			<div {...stylex.props(styles.content)}>
				<div {...stylex.props(styles.brand)}>
					<Zap size={16} {...stylex.props(styles.brandIcon)} />
					<span {...stylex.props(styles.brandName)}>mesh</span>
				</div>

				<div {...stylex.props(styles.card)}>
					<h1 {...stylex.props(styles.title)}>Sign in</h1>
					<p {...stylex.props(styles.description)}>
						Deploy services from GitHub repositories.
					</p>

					{error && (
						<div {...stylex.props([noticeStyles.error, styles.errorMessage])}>
							{loginErrorMessage(error, errorDetail)}
						</div>
					)}

					<div {...stylex.props(styles.loginMethods)}>
						{state.githubLoginEnabled && (
							<a
								href={buildGitHubAuthStartURL(
									state.publicBaseURL,
									redirectTo ?? "/",
								)}
								{...stylex.props(styles.githubLink)}
							>
								<div>
									<p {...stylex.props(styles.methodTitle)}>
										Continue with GitHub
									</p>
									<p {...stylex.props(styles.methodDescription)}>
										Access your repos and private images
									</p>
								</div>
								<span {...stylex.props(styles.methodArrow)}>→</span>
							</a>
						)}

						{state.devUsers.length > 0 && (
							<div {...stylex.props(styles.devLogins)}>
								<div {...stylex.props(styles.devLoginsHeader)}>
									<span {...stylex.props(styles.devLoginsLabel)}>
										Dev logins
									</span>
								</div>
								{state.devUsers.map((user) => (
									<a
										key={user.id}
										href={`/auth/callback?user_id=${encodeURIComponent(user.id)}&email=${encodeURIComponent(user.email)}&redirect=${encodeURIComponent(redirectTo ?? "/")}`}
										{...stylex.props(styles.devLoginLink)}
									>
										<div>
											<p {...stylex.props(styles.devEmail)}>{user.email}</p>
											<p {...stylex.props(styles.devUserId)}>{user.id}</p>
										</div>
										<span {...stylex.props(styles.methodArrow)}>→</span>
									</a>
								))}
							</div>
						)}

						{state.devUsers.length === 0 && !state.githubLoginEnabled && (
							<div {...stylex.props(styles.unconfiguredNotice)}>
								No sign-in methods are configured. Set{" "}
								<code {...stylex.props(styles.configVariable)}>
									DASHBOARD_DEV_USERS
								</code>{" "}
								or GitHub OAuth in the environment.
							</div>
						)}
					</div>
				</div>
			</div>
		</main>
	);
}
export function buildGitHubAuthStartURL(
	publicBaseURL: string,
	redirectTo: string,
): string {
	const url = new URL("/auth/start", publicBaseURL);
	url.searchParams.set("redirect", redirectTo);
	return url.toString();
}
export function loginErrorMessage(code: string, detail?: string): string {
	switch (code) {
		case "missing_verified_email":
			return "GitHub did not provide a verified primary email for this account.";
		case "email_linked_to_other_github":
			return "That email is already linked to a different GitHub account.";
		case "ambiguous_existing_user":
			return "Multiple existing users match that verified email.";
		case "invalid_signin_state":
			return "The sign-in request expired or was invalid. Try again.";
		case "github_auth_unavailable":
			return "GitHub sign-in is not configured for this environment.";
		case "dev_auth_unavailable":
			return "Dev login is not available in this environment.";
		case "invalid_dev_login":
			return "That dev login identity is not allowed.";
		case "github_api_error":
			return detail ?? "GitHub sign-in failed.";
		default:
			return "Sign-in failed.";
	}
}
