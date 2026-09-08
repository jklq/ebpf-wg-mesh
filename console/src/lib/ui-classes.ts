import { cn } from "./cn";

export const btnPrimary =
	"inline-flex cursor-pointer items-center gap-1.5 rounded-[1px] border-0 bg-accent px-3.5 py-[7px] font-condensed text-[11px] font-bold uppercase tracking-[0.09em] text-[#0f0e0d] transition-[filter,transform] duration-100 ease-out enabled:hover:brightness-110 enabled:active:translate-y-px disabled:cursor-not-allowed disabled:opacity-35";

export const btnSecondary =
	"inline-flex cursor-pointer items-center gap-1.5 rounded-[1px] border border-line bg-transparent px-3.5 py-[7px] font-condensed text-[11px] font-semibold uppercase tracking-[0.07em] text-ink transition-[border-color,background-color] duration-100 ease-out enabled:hover:border-line-bright enabled:hover:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-35";

export const btnGhost =
	"inline-flex cursor-pointer items-center gap-1.5 rounded-[1px] border-0 bg-transparent px-2 py-1.5 text-[11px] font-medium text-muted transition-[background-color,color] duration-100 ease-out hover:bg-surface-hover hover:text-ink disabled:cursor-not-allowed disabled:opacity-35";

export const btnDanger =
	"inline-flex cursor-pointer items-center gap-1.5 rounded-[1px] border border-failed bg-failed-dim px-3.5 py-[7px] font-condensed text-[11px] font-bold uppercase tracking-[0.07em] text-failed transition-colors duration-100 ease-out hover:bg-[rgba(184,66,66,0.22)]";

export const btnDangerSolid =
	"inline-flex cursor-pointer items-center gap-1.5 rounded-[1px] border border-failed bg-failed px-[18px] py-2 font-condensed text-[11px] font-bold uppercase tracking-[0.09em] text-[#f5eceb] transition-[filter,transform] duration-100 ease-out enabled:hover:brightness-110 enabled:active:translate-y-px disabled:cursor-not-allowed disabled:border-[rgba(184,66,66,0.4)] disabled:bg-[rgba(184,66,66,0.35)] disabled:text-[rgba(245,236,235,0.55)]";

export const btnDangerOutline =
	"inline-flex cursor-pointer items-center gap-1.5 whitespace-nowrap rounded-[1px] border border-[rgba(184,66,66,0.5)] bg-transparent px-3.5 py-[7px] font-condensed text-[11px] font-bold uppercase tracking-[0.07em] text-failed transition-[background-color,border-color] duration-100 ease-out enabled:hover:border-failed enabled:hover:bg-failed-dim disabled:cursor-not-allowed disabled:opacity-35";

export const iconBtn =
	"inline-flex size-[30px] cursor-pointer items-center justify-center rounded-[1px] border border-line bg-transparent text-muted enabled:hover:border-line-bright enabled:hover:bg-surface-hover enabled:hover:text-ink disabled:cursor-not-allowed disabled:opacity-35";

export const panelIconBtn =
	"inline-flex size-8 cursor-pointer items-center justify-center border border-transparent bg-transparent text-muted transition-[color,background-color,border-color] duration-100 ease-out hover:border-line hover:bg-surface-hover hover:text-ink disabled:cursor-not-allowed disabled:opacity-45";

export const fieldLabel =
	"mb-1.5 block font-condensed text-[11px] font-bold uppercase tracking-[0.11em] text-label";

export const fieldInput =
	"w-full appearance-none rounded-none border border-line bg-canvas px-2.5 py-2 font-mono text-[13px] text-ink outline-none transition-[border-color] duration-100 placeholder:text-dim focus:border-accent focus:shadow-[0_0_0_2px_var(--color-accent-dim)]";

export const fieldInputUnapplied =
	"w-full appearance-none rounded-none border border-[rgba(139,184,236,0.72)] bg-[rgba(74,121,178,0.11)] px-2.5 py-2 font-mono text-[13px] text-ink shadow-[inset_0_0_0_1px_rgba(139,184,236,0.18)] outline-none transition-[border-color] duration-100 placeholder:text-dim focus:border-unapplied focus:shadow-[inset_0_0_0_1px_rgba(139,184,236,0.24),0_0_0_2px_rgba(139,184,236,0.18)]";

export const unappliedSurface =
	"border-[rgba(139,184,236,0.72)] bg-[rgba(74,121,178,0.11)]";

export const errorMsg =
	"border border-[rgba(184,66,66,0.25)] border-l-[3px] border-l-failed bg-failed-dim px-2.5 py-2 font-mono text-xs text-failed";

export const successMsg =
	"border border-[rgba(109,190,130,0.32)] border-l-[3px] border-l-healthy bg-healthy-dim px-2.5 py-2 text-[13px] text-[#9ed6ab]";

export const modalOverlay =
	"fixed inset-0 z-[100] flex animate-fade-in items-start justify-center bg-[rgba(15,14,13,0.9)] px-6 py-12 backdrop-blur-[2px]";

export const modalCard =
	"max-h-[90vh] w-full max-w-[520px] animate-slide-down overflow-y-auto rounded-sm border border-line-bright bg-surface";

export const badge =
	"inline-flex items-center gap-1 rounded-[1px] px-1.5 py-0.5 font-condensed text-[10px] font-bold uppercase tracking-[0.09em]";

export const badgeHealthy = `${badge} border border-[rgba(92,170,112,0.22)] bg-healthy-dim text-healthy`;
export const badgeBuilding = `${badge} border border-[rgba(192,133,32,0.22)] bg-building-dim text-building`;
export const badgeFailed = `${badge} border border-[rgba(184,66,66,0.22)] bg-failed-dim text-failed`;
export const badgeOffline = `${badge} border border-line bg-[rgba(44,42,38,0.5)] text-muted`;
export const badgeEdited = `${badge} whitespace-nowrap border border-[rgba(139,184,236,0.28)] bg-[rgba(74,121,178,0.14)] text-unapplied`;

export const statusDot = "size-[7px] shrink-0 rounded-[1px]";

export function badgeClass(
	tone: "healthy" | "building" | "failed" | "offline" | "edited",
): string {
	switch (tone) {
		case "building":
			return badgeBuilding;
		case "failed":
			return badgeFailed;
		case "offline":
			return badgeOffline;
		case "edited":
			return badgeEdited;
		default:
			return badgeHealthy;
	}
}

export function statusDotClass(
	health: "healthy" | "building" | "failed" | "offline" | string,
): string {
	switch (health) {
		case "building":
			return cn(statusDot, "bg-building animate-pulse-building");
		case "failed":
			return cn(statusDot, "bg-failed");
		case "offline":
			return cn(statusDot, "bg-offline");
		default:
			return cn(statusDot, "bg-healthy");
	}
}

export const panelEyebrow =
	"font-condensed text-[11px] font-bold uppercase tracking-[0.1em] text-muted";
