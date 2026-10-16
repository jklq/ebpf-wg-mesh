import * as stylex from "@stylexjs/stylex";
import { Link } from "@tanstack/react-router";
import { buttonStyles } from "#/components/ui/button";
import { Notice } from "#/components/ui/notice";
import { PanelSection } from "#/components/ui/section";
import { PanelTabSkeleton } from "#/features/dashboard/service-panel/panel-skeleton";
import { UsageFreshness } from "#/features/usage/usage-status";
import { UsageSummary } from "#/features/usage/usage-summary";
import { useProjectUsage } from "#/features/usage/use-project-usage";
import type {
	DashboardProject,
	DashboardServiceRecord,
} from "#/lib/dashboard/core/types.server";

/** The service's usage over the last seven days. */
export function PanelUsage({
	service,
	project,
}: {
	service: DashboardServiceRecord;
	project: DashboardProject;
}) {
	const { usage, loadedAtMs, error } = useProjectUsage({
		projectId: project.id,
		serviceId: service.id,
		range: "7d",
	});

	return (
		<PanelSection
			title="Last 7 days"
			lede="Measured usage of every replica of this service, including replaced ones."
		>
			{error && <Notice>Could not load usage: {error}</Notice>}
			{usage ? (
				<>
					<UsageFreshness usage={usage} loadedAtMs={loadedAtMs} />
					<UsageSummary usage={usage} showVolumes={false} />
				</>
			) : (
				!error && <PanelTabSkeleton />
			)}
			<Link
				to="/projects/$projectId/usage"
				params={{ projectId: project.id }}
				{...stylex.props(buttonStyles.base, buttonStyles.ghost, styles.link)}
			>
				Project usage
			</Link>
		</PanelSection>
	);
}

const styles = stylex.create({ link: { alignSelf: "flex-start" } });
