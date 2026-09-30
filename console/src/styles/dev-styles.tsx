export function DevStyles() {
	// SSR needs the initial sheet; client modules inject updates without replacing it.
	return import.meta.env.DEV ? (
		<link rel="stylesheet" href="/virtual:stylex.css" />
	) : null;
}
