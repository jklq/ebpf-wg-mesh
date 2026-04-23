import type {
	ComponentType,
	CSSProperties,
	Dispatch,
	RefObject,
	SetStateAction,
} from "react";

import type { DashboardHomeState } from "#/lib/dashboard/core/types.server";

export type DashboardTab = "overview" | "deployments" | "settings" | "domains";

export type ServiceHealth = "healthy" | "building" | "failed" | "offline";

export type NewServiceStep = "repo" | "deploying";

export type InspectRepositoryFn = (input: {
	data: { repositorySelector: string };
}) => Promise<DashboardHomeState>;

export type ConfirmRepositoryFn = (input: {
	data: {
		repositorySelector: string;
		serviceName?: string;
		trackedRef?: string;
		dockerfilePath?: string;
		contextDir?: string;
	};
}) => Promise<unknown>;

export type PickerAction = {
	href: string;
	label: string;
	icon: ComponentType<{ size?: number; style?: CSSProperties }>;
	external?: boolean;
};

export type PickerRepository = {
	fullName: string;
};

export type RepositoryPickerProps = {
	actions: PickerAction[];
	error?: string;
	filteredRepositories: PickerRepository[];
	highlightedIndex: number;
	hoveredIndex: number | null;
	loading: boolean;
	onActivateIndex: (index: number) => void;
	onClose: () => void;
	onConfirm: (selector: string) => void;
	onRepositorySelect: (selector: string) => void;
	onSearchChange: () => void;
	repoListRef: RefObject<HTMLDivElement | null>;
	repoSearch: string;
	repoSelector: string;
	setHighlightedIndex: Dispatch<SetStateAction<number>>;
	setHoveredIndex: Dispatch<SetStateAction<number | null>>;
	setRepoSearch: Dispatch<SetStateAction<string>>;
	showEmptyState: boolean;
};
