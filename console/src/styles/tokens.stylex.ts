import * as stylex from "@stylexjs/stylex";

export const colors = stylex.defineVars({
	canvas: "#100e0c",
	surface: "#181613",
	surfaceRaised: "#221f1b",
	surfaceHover: "#2c2823",
	line: "#433e38",
	lineBright: "#6a6258",
	ink: "#f0e8dc",
	muted: "#b7aea2",
	dim: "#7d756b",
	label: "#cfc4b6",
	accent: "#e28a24",
	accentDim: "rgba(226, 138, 36, 0.14)",
	accentGlow: "rgba(226, 138, 36, 0.22)",
	healthy: "#6dbe82",
	healthyDim: "rgba(109, 190, 130, 0.12)",
	building: "#d49a2a",
	buildingDim: "rgba(212, 154, 42, 0.12)",
	failed: "#d05555",
	failedDim: "rgba(208, 85, 85, 0.12)",
	offline: "#5c564e",
	danger: "#d05555",
	unapplied: "#8bb8ec",
});

export const fonts = stylex.defineVars({
	sans: '"Barlow", system-ui, sans-serif',
	mono: '"IBM Plex Mono", monospace',
	display: '"IBM Plex Serif", "Iowan Old Style", Georgia, serif',
	condensed: '"Barlow Condensed", "Barlow", system-ui, sans-serif',
});

export const sizes = stylex.defineVars({
	header: "48px",
	sidePanel: "40vw",
});

export const space = stylex.defineVars({
	xs: "0.25rem",
	sm: "0.5rem",
	md: "0.75rem",
	lg: "1rem",
	xl: "1.5rem",
	xxl: "2rem",
});
export const shape = stylex.defineVars({ control: "1px", card: "4px" });
export const motion = stylex.defineVars({
	fast: "100ms",
	normal: "200ms",
	ease: "cubic-bezier(0, 0, 0.2, 1)",
});
