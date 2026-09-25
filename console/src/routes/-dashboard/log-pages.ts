import type { DashboardServiceLogPage } from "#/lib/dashboard/core/types.server";
import { fetchServiceLogs } from "./server-fns";

type LogQuery = NonNullable<
	NonNullable<Parameters<typeof fetchServiceLogs>[0]>["data"]
>;
export type LogCursors = Pick<
	DashboardServiceLogPage,
	"nextPageToken" | "nextGapPageToken"
>;

/** Exhausted streams are ignored while the other cursor continues forward. */
export async function fetchLogPage(
	data: LogQuery,
	previous?: LogCursors,
): Promise<DashboardServiceLogPage> {
	const page = await fetchServiceLogs({
		data: {
			...data,
			pageToken: previous?.nextPageToken,
			gapPageToken: previous?.nextGapPageToken,
		},
	});
	const linesDone = previous !== undefined && !previous.nextPageToken;
	const gapsDone = previous !== undefined && !previous.nextGapPageToken;
	if (
		(!linesDone &&
			page.nextPageToken &&
			page.nextPageToken === previous?.nextPageToken) ||
		(!gapsDone &&
			page.nextGapPageToken &&
			page.nextGapPageToken === previous?.nextGapPageToken)
	)
		throw new Error("Log pagination did not advance.");
	return {
		lines: linesDone ? [] : page.lines,
		gaps: gapsDone ? [] : page.gaps,
		nextPageToken: linesDone ? undefined : page.nextPageToken,
		nextGapPageToken: gapsDone ? undefined : page.nextGapPageToken,
	};
}
