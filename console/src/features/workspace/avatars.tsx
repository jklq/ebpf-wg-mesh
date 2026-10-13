import * as stylex from "@stylexjs/stylex";
import { colors, fonts, shape } from "#/styles/tokens.stylex";

/** A person's GitHub avatar, or their initial when they have no GitHub login. */
export function UserAvatar({
	email,
	githubLogin,
}: {
	email?: string;
	githubLogin?: string;
}) {
	if (githubLogin) {
		return (
			<img
				src={`https://github.com/${encodeURIComponent(githubLogin)}.png?size=56`}
				alt=""
				width={28}
				height={28}
				{...stylex.props(styles.avatar, styles.round)}
			/>
		);
	}
	return (
		<span
			aria-hidden
			{...stylex.props(styles.avatar, styles.round, styles.initial)}
		>
			{initial(email)}
		</span>
	);
}

/** A workspace's square mark: its initial on an accent tile. */
export function WorkspaceMark({
	name,
	large = false,
}: {
	name: string;
	large?: boolean;
}) {
	return (
		<span
			aria-hidden
			{...stylex.props(styles.avatar, styles.mark, large && styles.largeMark)}
		>
			{initial(name)}
		</span>
	);
}

function initial(value: string | undefined): string {
	return (value?.trim()[0] ?? "?").toUpperCase();
}

const styles = stylex.create({
	avatar: {
		display: "inline-flex",
		width: "28px",
		height: "28px",
		flexShrink: "0",
		alignItems: "center",
		justifyContent: "center",
		objectFit: "cover",
		fontFamily: fonts.condensed,
		fontSize: "13px",
		fontWeight: "700",
	},
	round: { borderRadius: "50%" },
	initial: {
		backgroundColor: colors.surfaceHover,
		color: colors.label,
	},
	mark: {
		width: "26px",
		height: "26px",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.accentGlow,
		backgroundColor: colors.accentDim,
		color: colors.accent,
	},
	largeMark: { width: "40px", height: "40px", fontSize: "18px" },
});
