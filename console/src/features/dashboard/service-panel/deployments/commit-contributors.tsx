import * as stylex from "@stylexjs/stylex";
import { useState } from "react";
import type { DashboardBuildStatus } from "#/lib/dashboard/core/types.server";
import { colors, fonts } from "#/styles/tokens.stylex";

type Contributor = NonNullable<
	DashboardBuildStatus["commitContributors"]
>[number];

const MAX_VISIBLE = 3;

// CommitContributors stacks the avatars of a commit's author and co-authors,
// author on top.
export function CommitContributors({
	contributors,
	size = "md",
}: {
	contributors: readonly Contributor[] | undefined;
	size?: "sm" | "md";
}) {
	if (!contributors?.length) return null;
	const visible = contributors.slice(0, MAX_VISIBLE);
	const hidden = contributors.length - visible.length;
	const names = contributors.map(contributorName).join(", ");
	return (
		<span
			role="img"
			aria-label={names}
			title={names}
			{...stylex.props(styles.stack)}
		>
			{visible.map((contributor, index) => (
				<Avatar
					key={contributor.login || contributor.name || index}
					contributor={contributor}
					size={size}
					depth={visible.length - index}
					first={index === 0}
				/>
			))}
			{hidden > 0 && (
				<span
					{...stylex.props(
						styles.avatar,
						styles.overflow,
						size === "sm" ? styles.small : styles.medium,
						size === "sm" ? styles.overlapSmall : styles.overlap,
					)}
				>
					+{hidden}
				</span>
			)}
		</span>
	);
}

function Avatar({
	contributor,
	size,
	depth,
	first,
}: {
	contributor: Contributor;
	size: "sm" | "md";
	depth: number;
	first: boolean;
}) {
	const [failed, setFailed] = useState(false);
	const layer = [
		styles.avatar,
		size === "sm" ? styles.small : styles.medium,
		!first && (size === "sm" ? styles.overlapSmall : styles.overlap),
		styles.depth(depth),
	];
	if (contributor.avatarUrl && !failed) {
		return (
			<img
				src={contributor.avatarUrl}
				alt=""
				loading="lazy"
				onError={() => setFailed(true)}
				{...stylex.props(layer)}
			/>
		);
	}
	return (
		<span {...stylex.props(layer, styles.initials)}>
			{initials(contributorName(contributor))}
		</span>
	);
}

function contributorName(contributor: Contributor): string {
	return contributor.name || contributor.login || "Unknown";
}

function initials(name: string): string {
	const words = name.trim().split(/\s+/).filter(Boolean);
	const letters =
		words.length > 1
			? words[0][0] + words[words.length - 1][0]
			: name.slice(0, 2);
	return letters.toUpperCase();
}

const styles = stylex.create({
	stack: { display: "inline-flex", flexShrink: 0, alignItems: "center" },
	avatar: {
		position: "relative",
		display: "inline-flex",
		flexShrink: 0,
		alignItems: "center",
		justifyContent: "center",
		borderRadius: 999,
		borderStyle: "solid",
		borderWidth: 2,
		borderColor: colors.surface,
		backgroundColor: colors.surfaceHover,
		objectFit: "cover",
	},
	medium: { width: 28, height: 28, fontSize: 10 },
	small: { width: 18, height: 18, borderWidth: 1.5, fontSize: 7 },
	overlap: { marginLeft: -8 },
	overlapSmall: { marginLeft: -5 },
	depth: (depth: number) => ({ zIndex: depth }),
	initials: {
		fontFamily: fonts.sans,
		fontWeight: 700,
		color: colors.label,
	},
	overflow: {
		fontFamily: fonts.mono,
		color: colors.muted,
	},
});
