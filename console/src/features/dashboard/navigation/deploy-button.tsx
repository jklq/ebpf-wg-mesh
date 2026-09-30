import * as stylex from "@stylexjs/stylex";
import { Github, Plus } from "lucide-react";
import type { MouseEvent } from "react";
import { Button, buttonStyles } from "#/components/ui/button";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

export function DeployButton({
	state,
	onNewService,
	onPreload,
	stopPropagation,
}: {
	state: DashboardHomeState;
	onNewService: () => void;
	onPreload?: () => void;
	stopPropagation?: boolean;
}) {
	const stop = (e: MouseEvent) => e.stopPropagation();

	if (!state.githubAccount && state.githubLoginURL) {
		return (
			<a
				href={state.githubLoginURL}
				{...stylex.props(buttonStyles.base, buttonStyles.primary)}
				onMouseDown={stopPropagation ? stop : undefined}
				onClick={stopPropagation ? stop : undefined}
			>
				<Github size={13} />
				Connect GitHub to deploy
			</a>
		);
	}

	return (
		<Button
			type="button"
			variant="primary"
			onFocus={onPreload}
			onMouseEnter={onPreload}
			onPointerDown={onPreload}
			onMouseDown={stopPropagation ? stop : undefined}
			onClick={
				stopPropagation
					? (e) => {
							e.stopPropagation();
							onNewService();
						}
					: onNewService
			}
		>
			<Plus size={13} />
			Deploy service
		</Button>
	);
}
