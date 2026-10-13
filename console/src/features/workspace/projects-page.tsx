import * as stylex from "@stylexjs/stylex";
import { Link, useRouter } from "@tanstack/react-router";
import { Layers, Loader2, Plus, Search } from "lucide-react";
import { useState } from "react";
import { Button } from "#/components/ui/button";
import { TextInput } from "#/components/ui/field";
import { Notice } from "#/components/ui/notice";
import { StatusDot } from "#/components/ui/status-dot";
import { WorkspacePageHeader } from "#/features/workspace/page-header";
import { canEditProjects } from "#/features/workspace/roles";
import { WorkspaceShell } from "#/features/workspace/workspace-shell";
import type {
	DashboardProjectSummary,
	DashboardWorkspaceShell,
} from "#/lib/dashboard/core/types.server";
import { doCreateWorkspaceProject } from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, motion, shape, space } from "#/styles/tokens.stylex";

export function ProjectsPage({
	shell,
	projects,
}: {
	shell: DashboardWorkspaceShell;
	projects: Array<DashboardProjectSummary>;
}) {
	const router = useRouter();
	const [search, setSearch] = useState("");
	const [creating, setCreating] = useState(false);
	const [error, setError] = useState<string>();
	const canCreate = canEditProjects(shell.workspace);
	const query = search.trim().toLowerCase();
	const visible = projects.filter((entry) =>
		entry.project.name.toLowerCase().includes(query),
	);

	const createProject = async () => {
		setCreating(true);
		setError(undefined);
		try {
			const created = await doCreateWorkspaceProject({
				data: { workspaceId: shell.workspace.id },
			});
			await router.navigate({
				to: "/environments/$environmentId",
				params: { environmentId: created.environmentId },
			});
		} catch (cause) {
			setError(formatError(cause));
			setCreating(false);
		}
	};

	const newProjectButton = canCreate && (
		<Button
			variant="primary"
			disabled={creating}
			onClick={() => void createProject()}
		>
			{creating ? (
				<Loader2 size={13} {...stylex.props(styles.spinner)} />
			) : (
				<Plus size={13} />
			)}
			New project
		</Button>
	);

	return (
		<WorkspaceShell shell={shell} section="projects">
			<WorkspacePageHeader
				title="Projects"
				lede={`${projects.length} ${projects.length === 1 ? "project" : "projects"} in ${shell.workspace.name}`}
				actions={
					<>
						{projects.length > 0 && (
							<div {...stylex.props(styles.search)}>
								<Search size={13} {...stylex.props(styles.searchIcon)} />
								<TextInput
									type="search"
									aria-label="Search projects"
									placeholder="Search projects"
									value={search}
									onChange={(event) => setSearch(event.target.value)}
									styles={styles.searchInput}
								/>
							</div>
						)}
						{newProjectButton}
					</>
				}
			/>
			{error && <Notice>{error}</Notice>}
			{projects.length === 0 ? (
				<div {...stylex.props(styles.empty)}>
					<Layers size={22} {...stylex.props(styles.emptyIcon)} />
					<h2 {...stylex.props(styles.emptyTitle)}>No projects yet</h2>
					<p {...stylex.props(styles.emptyText)}>
						{canCreate
							? "A project holds environments and the services you deploy into them."
							: "An admin or member of this workspace can create the first project."}
					</p>
					{newProjectButton}
				</div>
			) : visible.length === 0 ? (
				<p {...stylex.props(styles.emptyText)}>
					No projects match “{search.trim()}”.
				</p>
			) : (
				<ul {...stylex.props(styles.grid)}>
					{visible.map((summary) => (
						<li key={summary.project.id}>
							<ProjectCard summary={summary} />
						</li>
					))}
				</ul>
			)}
		</WorkspaceShell>
	);
}

const maxServiceChips = 4;

function ProjectCard({ summary }: { summary: DashboardProjectSummary }) {
	const { project, services, landingEnvironment, environmentCount } = summary;
	const online = services.filter((service) => service.online).length;
	const hidden = services.length - maxServiceChips;
	return (
		<Link
			to="/projects/$projectId"
			params={{ projectId: project.id }}
			{...stylex.props(styles.card)}
		>
			<div {...stylex.props(styles.cardHeader)}>
				<h2 {...stylex.props(styles.cardTitle)}>{project.name}</h2>
				{environmentCount > 1 && (
					<span {...stylex.props(styles.environmentCount)}>
						{environmentCount} environments
					</span>
				)}
			</div>
			<div {...stylex.props(styles.cardBody)}>
				{services.length === 0 ? (
					<span {...stylex.props(styles.noServices)}>No services yet</span>
				) : (
					<ul {...stylex.props(styles.chips)}>
						{services.slice(0, maxServiceChips).map((service) => (
							<li key={service.id} {...stylex.props(styles.chip)}>
								<StatusDot health={service.online ? "healthy" : "offline"} />
								<span {...stylex.props(styles.chipName)}>{service.name}</span>
							</li>
						))}
						{hidden > 0 && (
							<li {...stylex.props(styles.chip, styles.moreChip)}>
								+{hidden} more
							</li>
						)}
					</ul>
				)}
			</div>
			<footer {...stylex.props(styles.cardFooter)}>
				<StatusDot
					health={
						services.length === 0
							? "offline"
							: online === services.length
								? "healthy"
								: online === 0
									? "failed"
									: "building"
					}
				/>
				<span>{landingEnvironment?.name ?? "No environments"}</span>
				{services.length > 0 && (
					<>
						<span aria-hidden {...stylex.props(styles.dot)}>
							·
						</span>
						<span>
							{online}/{services.length}{" "}
							{services.length === 1 ? "service" : "services"} online
						</span>
					</>
				)}
			</footer>
		</Link>
	);
}

const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });

const styles = stylex.create({
	spinner: { animation: `${spin} 1s linear infinite` },
	search: { position: "relative", display: "flex", alignItems: "center" },
	searchIcon: {
		position: "absolute",
		left: "0.625rem",
		color: colors.dim,
		pointerEvents: "none",
	},
	searchInput: { width: "15rem", paddingLeft: "2rem" },
	grid: {
		margin: 0,
		display: "grid",
		gridTemplateColumns: "repeat(auto-fill, minmax(17.5rem, 1fr))",
		gap: space.lg,
		listStyleType: "none",
		padding: 0,
	},
	card: {
		display: "flex",
		height: "100%",
		flexDirection: "column",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: {
			default: colors.line,
			":hover": { default: null, "@media (hover: hover)": colors.lineBright },
			":focus-visible": colors.accent,
		},
		backgroundColor: colors.surface,
		color: colors.ink,
		textDecoration: "none",
		transitionProperty: "border-color, transform",
		transitionDuration: motion.normal,
		transitionTimingFunction: motion.ease,
		transform: {
			default: null,
			":hover": { default: null, "@media (hover: hover)": "translateY(-1px)" },
		},
	},
	cardHeader: {
		display: "flex",
		alignItems: "baseline",
		justifyContent: "space-between",
		gap: space.sm,
		paddingInline: space.lg,
		paddingTop: space.lg,
	},
	cardTitle: {
		margin: 0,
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
		fontFamily: fonts.display,
		fontSize: "19px",
		fontWeight: "500",
	},
	environmentCount: {
		flexShrink: 0,
		fontFamily: fonts.mono,
		fontSize: "11px",
		color: colors.dim,
	},
	cardBody: {
		display: "flex",
		minHeight: "7.5rem",
		flex: 1,
		alignItems: "center",
		justifyContent: "center",
		margin: space.md,
		borderRadius: shape.card,
		backgroundColor: colors.canvas,
		backgroundImage: `radial-gradient(${colors.line} 1px, transparent 1px)`,
		backgroundSize: "14px 14px",
		padding: space.md,
	},
	noServices: {
		fontFamily: fonts.condensed,
		fontSize: "11px",
		fontWeight: "700",
		textTransform: "uppercase",
		letterSpacing: "0.1em",
		color: colors.dim,
	},
	chips: {
		margin: 0,
		display: "flex",
		flexWrap: "wrap",
		justifyContent: "center",
		gap: space.sm,
		listStyleType: "none",
		padding: 0,
	},
	chip: {
		display: "inline-flex",
		maxWidth: "10rem",
		alignItems: "center",
		gap: "0.375rem",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: colors.line,
		backgroundColor: colors.surfaceRaised,
		paddingInline: space.sm,
		paddingBlock: space.xs,
		fontSize: "12px",
	},
	chipName: {
		overflow: "hidden",
		textOverflow: "ellipsis",
		whiteSpace: "nowrap",
	},
	moreChip: { color: colors.muted },
	cardFooter: {
		display: "flex",
		alignItems: "center",
		gap: space.sm,
		paddingInline: space.lg,
		paddingBottom: space.md,
		fontSize: "12px",
		color: colors.muted,
	},
	dot: { color: colors.dim },
	empty: {
		display: "flex",
		flexDirection: "column",
		alignItems: "center",
		gap: space.md,
		borderStyle: "dashed",
		borderWidth: "1px",
		borderColor: colors.line,
		paddingInline: space.xl,
		paddingBlock: "4rem",
		textAlign: "center",
	},
	emptyIcon: { color: colors.accent },
	emptyTitle: {
		margin: 0,
		fontFamily: fonts.display,
		fontSize: "22px",
		fontWeight: "500",
	},
	emptyText: {
		margin: 0,
		maxWidth: "26rem",
		fontSize: "13px",
		color: colors.muted,
	},
});
