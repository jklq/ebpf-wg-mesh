import type * as stylex from "@stylexjs/stylex";
import { Box, Database, Globe, Layers, Server } from "lucide-react";
import type { ComponentType } from "react";
import type {
	DashboardDeletedResource,
	DashboardDeletedResourceKind,
} from "#/lib/dashboard/core/types.server";

export const kindMeta: Record<
	DashboardDeletedResourceKind,
	{
		label: string;
		icon: ComponentType<{ size?: number; styles?: stylex.StyleXStyles }>;
	}
> = {
	project: { label: "Project", icon: Layers },
	environment: { label: "Environment", icon: Server },
	service: { label: "Service", icon: Box },
	domain: { label: "Domain", icon: Globe },
	volume: { label: "Volume", icon: Database },
};
export function resourceKey(entry: { kind: string; id: string }): string {
	return `${entry.kind}:${entry.id}`;
}
export function groupResources(resources: Array<DashboardDeletedResource>) {
	const keys = new Set(resources.map(resourceKey));
	const childrenOf = new Map<string, Array<DashboardDeletedResource>>();
	const projects: Array<{
		projectId: string;
		projectName: string;
		roots: Array<DashboardDeletedResource>;
	}> = [];
	for (const raw of resources) {
		const entry = raw;
		const parentKey = entry.deletedWith
			? resourceKey(entry.deletedWith)
			: undefined;
		if (parentKey && keys.has(parentKey)) {
			childrenOf.set(parentKey, [...(childrenOf.get(parentKey) ?? []), entry]);
			continue;
		}
		let group = projects.find((item) => item.projectId === entry.projectId);
		if (!group) {
			group = {
				projectId: entry.projectId,
				projectName: entry.projectName,
				roots: [],
			};
			projects.push(group);
		}
		group.roots.push(entry);
	}
	return { projects, childrenOf };
}
