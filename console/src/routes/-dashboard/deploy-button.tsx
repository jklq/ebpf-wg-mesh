import { Github, Plus } from "lucide-react";
import type { MouseEvent } from "react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";
import { btnPrimary } from "#/lib/ui-classes";

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
				className={btnPrimary}
				onMouseDown={stopPropagation ? stop : undefined}
				onClick={stopPropagation ? stop : undefined}
			>
				<Github size={13} />
				Connect GitHub to deploy
			</a>
		);
	}

	return (
		<button
			type="button"
			className={btnPrimary}
			onFocus={onPreload}
			onMouseEnter={onPreload}
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
		</button>
	);
}
