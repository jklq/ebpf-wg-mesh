import * as stylex from "@stylexjs/stylex";
import type { ReactNode } from "react";
import { textStyles } from "#/components/ui/text";
import {
	finiteDouble,
	formatAverageBytes,
	formatByteCount,
	formatByteHours,
	formatCpuTime,
	formatVcpu,
	hasUsageData,
} from "#/features/usage/usage-model";
import type { DashboardProjectUsage } from "#/lib/dashboard/core/types.server";
import { colors, fonts, space } from "#/styles/tokens.stylex";

export const MEMORY_DEFINITION =
	"Total cgroup memory, including page cache and kernel memory.";

/** Range totals per meter. Without reported usage, values read "No data", never zero. */
export function UsageSummary({
	usage,
	showVolumes = true,
}: {
	usage: DashboardProjectUsage;
	showVolumes?: boolean;
}) {
	const total = usage.total;
	const reported = hasUsageData(usage) && total !== undefined;
	const average = (value: number | undefined, format: (n: number) => string) =>
		reported && value !== undefined ? format(value) : undefined;
	const retained = total?.retainedVolumeByteMilliseconds ?? "0";

	return (
		<div {...stylex.props(styles.grid)}>
			<SummaryCard
				label="CPU"
				value={average(finiteDouble(total?.averageVcpu), formatVcpu)}
				detail={
					reported &&
					`${formatCpuTime(total.cpuMicroseconds)} CPU time in total`
				}
				caption="Average over the range."
			/>
			<SummaryCard
				label="Memory"
				value={average(
					finiteDouble(total?.averageMemoryBytes),
					formatAverageBytes,
				)}
				detail={
					reported &&
					`${formatByteHours(total.memoryByteMilliseconds)} in total`
				}
				caption={MEMORY_DEFINITION}
			/>
			{showVolumes && (
				<SummaryCard
					label="Volumes"
					value={average(
						finiteDouble(total?.averageVolumeBytes),
						formatAverageBytes,
					)}
					detail={
						reported &&
						`${formatByteHours(total.volumeByteMilliseconds)} in total${
							retained !== "0"
								? `, ${formatByteHours(retained)} retained after deletion`
								: ""
						}`
					}
					caption="Used filesystem blocks, including metadata."
				/>
			)}
			<SummaryCard
				label="Public output"
				value={reported ? formatByteCount(total.publicEgressBytes) : undefined}
				detail={
					reported &&
					`${formatByteCount(total.publicDirectBytes)} direct · ${formatByteCount(total.publicIngressBytes)} via ingress`
				}
				caption="Application bytes sent to the internet, before TLS."
			/>
		</div>
	);
}

function SummaryCard({
	label,
	value,
	detail,
	caption,
}: {
	label: string;
	value: string | undefined;
	detail: ReactNode;
	caption: string;
}) {
	return (
		<section {...stylex.props(styles.card)}>
			<h3 {...stylex.props(textStyles.eyebrow, styles.label)}>{label}</h3>
			<p {...stylex.props(styles.value, value === undefined && styles.missing)}>
				{value ?? "No data"}
			</p>
			{detail && <p {...stylex.props(styles.detail)}>{detail}</p>}
			<p {...stylex.props(styles.caption)}>{caption}</p>
		</section>
	);
}

const styles = stylex.create({
	grid: {
		display: "grid",
		gridTemplateColumns: "repeat(auto-fit, minmax(13rem, 1fr))",
		gap: space.md,
	},
	card: {
		display: "flex",
		flexDirection: "column",
		gap: space.xs,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surface,
		padding: space.lg,
	},
	label: { margin: "0rem" },
	value: {
		margin: "0rem",
		fontFamily: fonts.display,
		fontSize: "26px",
		fontWeight: "500",
		letterSpacing: "-0.02em",
		color: colors.ink,
	},
	missing: { color: colors.dim },
	detail: {
		margin: "0rem",
		fontFamily: fonts.mono,
		fontSize: "12px",
		color: colors.label,
	},
	caption: {
		marginTop: "auto",
		marginBottom: "0rem",
		paddingTop: space.xs,
		fontSize: "12px",
		lineHeight: "1.4",
		color: colors.muted,
	},
});
