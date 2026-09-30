import { useEffect } from "react";

export function DevStyles() {
	useEffect(() => {
		if (import.meta.env.DEV) void import("virtual:stylex:css-only");
	}, []);

	return import.meta.env.DEV ? (
		<link rel="stylesheet" href="/virtual:stylex.css" />
	) : null;
}
