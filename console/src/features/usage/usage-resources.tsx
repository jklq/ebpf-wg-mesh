import * as stylex from "@stylexjs/stylex";
import {
	finiteDouble,
	formatAverageBytes,
	formatByteCount,
	formatCpuTime,
	formatVcpu,
} from "#/features/usage/usage-model";
import type { DashboardUsageResource } from "#/lib/dashboard/core/types.server";
import { formatDateTime } from "#/lib/time";
import { colors, fonts, space } from "#/styles/tokens.stylex";

/** Every resource with usage in the range, including deleted ones. */
export function UsageResources({
	resources,
}: {
	resources: DashboardUsageResource[];
}) {
	if (resources.length === 0)
		return <p {...stylex.props(styles.empty)}>No resources reported usage.</p>;
	return (
		<div {...stylex.props(styles.scroller)}>
			<table {...stylex.props(styles.table)}>
				<thead>
					<tr>
						<th {...stylex.props(styles.heading)}>Resource</th>
						<th {...stylex.props(styles.heading)}>Environment</th>
						<th {...stylex.props(styles.heading, styles.number)}>CPU time</th>
						<th {...stylex.props(styles.heading, styles.number)}>Avg CPU</th>
						<th {...stylex.props(styles.heading, styles.number)}>Avg memory</th>
						<th {...stylex.props(styles.heading, styles.number)}>Avg volume</th>
						<th {...stylex.props(styles.heading, styles.number)}>
							Public output
						</th>
					</tr>
				</thead>
				<tbody>
					{resources.map((resource) => (
						<ResourceRow
							key={`${resource.kind}/${resource.resourceId}`}
							resource={resource}
						/>
					))}
				</tbody>
			</table>
		</div>
	);
}

function ResourceRow({ resource }: { resource: DashboardUsageResource }) {
	const usage = resource.usage;
	const allocation = resource.kind === "allocation";
	const average = (value: number | undefined, format: (n: number) => string) =>
		value === undefined ? "—" : format(value);
	return (
		<tr>
			<td {...stylex.props(styles.cell)}>
				<div {...stylex.props(styles.name)}>
					{resource.name || resource.resourceId}
				</div>
				<div {...stylex.props(styles.meta)}>
					{allocation ? "Replica" : "Volume"} {resource.resourceId.slice(0, 8)}
					{resource.deletedAt && (
						<span {...stylex.props(styles.deleted)}>
							Deleted {formatDateTime(resource.deletedAt)}
						</span>
					)}
				</div>
			</td>
			<td {...stylex.props(styles.cell)}>{resource.environmentName || "—"}</td>
			<td {...stylex.props(styles.cell, styles.number)}>
				{allocation && usage ? formatCpuTime(usage.cpuMicroseconds) : "—"}
			</td>
			<td {...stylex.props(styles.cell, styles.number)}>
				{allocation
					? average(finiteDouble(usage?.averageVcpu), formatVcpu)
					: "—"}
			</td>
			<td {...stylex.props(styles.cell, styles.number)}>
				{allocation
					? average(finiteDouble(usage?.averageMemoryBytes), formatAverageBytes)
					: "—"}
			</td>
			<td {...stylex.props(styles.cell, styles.number)}>
				{allocation
					? "—"
					: average(
							finiteDouble(usage?.averageVolumeBytes),
							formatAverageBytes,
						)}
			</td>
			<td {...stylex.props(styles.cell, styles.number)}>
				{allocation && usage ? formatByteCount(usage.publicEgressBytes) : "—"}
			</td>
		</tr>
	);
}

const styles = stylex.create({
	empty: { margin: "0rem", fontSize: "13px", color: colors.muted },
	scroller: {
		overflowX: "auto",
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
	},
	table: { width: "100%", borderCollapse: "collapse", fontSize: "13px" },
	heading: {
		paddingInline: space.md,
		paddingBlock: space.sm,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderBottomColor: colors.line,
		textAlign: "left",
		whiteSpace: "nowrap",
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		letterSpacing: "0.08em",
		textTransform: "uppercase",
		color: colors.muted,
	},
	cell: {
		paddingInline: space.md,
		paddingBlock: space.sm,
		borderBottomStyle: "solid",
		borderBottomWidth: "1px",
		borderBottomColor: colors.line,
		verticalAlign: "top",
		color: colors.label,
	},
	number: {
		textAlign: "right",
		whiteSpace: "nowrap",
		fontFamily: fonts.mono,
		fontSize: "12px",
	},
	name: { fontWeight: "600", color: colors.ink },
	meta: {
		display: "flex",
		flexWrap: "wrap",
		gap: space.sm,
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.dim,
	},
	deleted: { color: colors.failed },
});
