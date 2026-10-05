import * as stylex from "@stylexjs/stylex";
import { Github, HardDrive, Plus } from "lucide-react";
import { useRef } from "react";
import {
	PaletteCrumbs,
	type PaletteItem,
	PaletteList,
	paletteStyles,
} from "#/components/ui/command-palette";
import { Dialog } from "#/components/ui/dialog";
import { NewServicePicker } from "#/features/dashboard/services/new-service-picker";
import type { useDashboardController } from "#/features/dashboard/use-dashboard-controller";
import { VolumeMountFlow } from "#/features/dashboard/volumes/volume-dialogs";
import {
	DEFAULT_VOLUME_SIZE_GIB,
	suggestVolumeName,
} from "#/features/dashboard/volumes/volume-model";
import { gibToBytes } from "#/lib/bytes";
import type {
	DashboardHomeState,
	DashboardVolume,
} from "#/lib/dashboard/core/types.server";
import {
	doCreateVolume,
	doUpdateService,
} from "#/lib/dashboard/server-functions";
import { formatError } from "#/lib/errors";
import { colors, fonts, shape, sizes } from "#/styles/tokens.stylex";

export type AddMenuStep = "root" | "repository" | "volume";

type Controller = ReturnType<typeof useDashboardController>;

export function AddButton({
	onOpen,
	onPreload,
}: {
	onOpen: () => void;
	onPreload: () => void;
}) {
	return (
		<button
			type="button"
			onClick={onOpen}
			onFocus={onPreload}
			onMouseEnter={onPreload}
			{...stylex.props(styles.addButton, styles.anchor)}
		>
			<Plus size={15} {...stylex.props(styles.addIcon)} />
			Add
		</button>
	);
}

export function AddMenu({
	state,
	panelOpen,
	creation,
	volumes,
}: {
	state: DashboardHomeState;
	panelOpen: boolean;
	creation: Controller["creation"];
	volumes: Controller["volumes"];
}) {
	const step = creation.step;
	if (!step) return null;
	return (
		<Dialog
			label="Add to canvas"
			onClose={creation.close}
			styles={[styles.overlay, panelOpen && styles.overlayBesidePanel]}
		>
			<div {...stylex.props(paletteStyles.card, styles.card)}>
				{step === "root" && (
					<RootStep
						state={state}
						canAddVolume={Boolean(volumes.environmentId)}
						onPick={creation.goTo}
					/>
				)}
				{step === "repository" && (
					<>
						<PaletteCrumbs crumbs={["GitHub Repository"]} />
						<NewServicePicker
							state={state}
							catalogLoading={creation.catalogLoading}
							initialError={creation.error}
							onClose={creation.close}
							onBack={() => creation.goTo("root")}
							onCreated={creation.onCreated}
							onCreating={creation.onCreating}
							onCreateFailed={creation.onFailed}
						/>
					</>
				)}
				{step === "volume" && (
					<VolumeStep
						volumes={volumes}
						onBack={() => creation.goTo("root")}
						onDone={creation.close}
					/>
				)}
			</div>
		</Dialog>
	);
}

function RootStep({
	state,
	canAddVolume,
	onPick,
}: {
	state: DashboardHomeState;
	canAddVolume: boolean;
	onPick: (step: AddMenuStep) => void;
}) {
	const needsGitHub = !state.githubAccount && Boolean(state.githubLoginURL);
	const items: Array<PaletteItem> = [
		{
			id: "repository",
			label: needsGitHub ? "Connect GitHub to deploy" : "GitHub Repository",
			icon: Github,
			next: !needsGitHub,
			href: needsGitHub ? state.githubLoginURL : undefined,
		},
	];
	if (canAddVolume && state.controlPlaneReachable) {
		items.push({ id: "volume", label: "Volume", icon: HardDrive, next: true });
	}
	return (
		<PaletteList
			placeholder="What would you like to create?"
			items={items}
			emptyText="Nothing matches your search"
			onPick={(item) => onPick(item.id as AddMenuStep)}
		/>
	);
}

/** Creates a fixed-size volume and mounts it on the chosen service. */
function VolumeStep({
	volumes,
	onBack,
	onDone,
}: {
	volumes: Controller["volumes"];
	onBack: () => void;
	onDone: () => void;
}) {
	// Keep a created volume across retries so a failed mount does not
	// leave a trail of orphaned volumes behind.
	const createdRef = useRef<DashboardVolume>(undefined);
	const environmentId = volumes.environmentId;
	if (!environmentId) return null;

	const submit = async (serviceId: string, mountPath: string) => {
		let created = createdRef.current;
		if (!created) {
			created = await doCreateVolume({
				data: {
					environmentId,
					name: suggestVolumeName(volumes.all.map((volume) => volume.name)),
					sizeBytes: gibToBytes(DEFAULT_VOLUME_SIZE_GIB),
				},
			});
			createdRef.current = created;
			volumes.upsert(created);
		}
		try {
			volumes.serviceUpdated(
				await doUpdateService({
					data: {
						serviceId,
						volumeMount: { volumeName: created.name, mountPath },
					},
				}),
			);
		} catch (cause) {
			throw new Error(
				`Volume ${created.name} created, but mounting failed: ${formatError(cause)}`,
			);
		}
		volumes.select(created.id);
		onDone();
	};

	return (
		<VolumeMountFlow
			crumb="Attach volume to service"
			services={volumes.services}
			volumes={volumes.all}
			onBack={onBack}
			onSubmit={submit}
		/>
	);
}

const besidePanel = `calc(${sizes.sidePanel} + 1.125rem)`;

const styles = stylex.create({
	anchor: {
		position: "absolute",
		top: "1.125rem",
		right: "1.125rem",
		zIndex: "36",
	},
	addButton: {
		display: "inline-flex",
		alignItems: "center",
		gap: "0.375rem",
		cursor: "pointer",
		borderRadius: shape.card,
		borderStyle: "solid",
		borderWidth: "1px",
		borderColor: { default: colors.line, ":hover": colors.lineBright },
		backgroundColor: {
			default: colors.surfaceRaised,
			":hover": colors.surfaceHover,
		},
		paddingInline: "0.875rem",
		paddingBlock: "0.5rem",
		fontFamily: fonts.sans,
		fontSize: "14px",
		fontWeight: "500",
		color: colors.ink,
		boxShadow: "0 8px 24px rgba(0,0,0,0.35)",
		transitionProperty: "background-color, border-color",
		transitionDuration: "100ms",
	},
	addIcon: { color: colors.muted },
	overlay: {
		justifyContent: "flex-end",
		paddingTop: `calc(${sizes.header} + 1.125rem)`,
		paddingRight: "1.125rem",
		backgroundColor: "rgba(15,14,13,0.35)",
		backdropFilter: "none",
	},
	overlayBesidePanel: {
		paddingRight: {
			default: besidePanel,
			"@media (width < 900px)": "1.125rem",
		},
	},
	card: { maxWidth: "400px" },
});
