import { fileURLToPath } from "node:url";
import stylex from "@stylexjs/unplugin";
import type { Plugin } from "vite";

const rootDir = fileURLToPath(new URL("../", import.meta.url));

// Keep the client, SSR and test transforms identical so token identities match.
export function stylexPlugin({ test = false } = {}): Plugin {
	const plugin: Plugin = stylex.vite({
		devMode: "css-only",
		useCSSLayers: { before: ["reset"], after: ["accessibility"] },
		runtimeInjection: false,
		aliases: { "#/*": [fileURLToPath(new URL("../src/*", import.meta.url))] },
		unstable_moduleResolution: { type: "commonJS", rootDir },
	});
	// Vitest needs compilation, but no CSS server or HMR polling interval.
	if (test) return { ...plugin, configureServer: undefined };
	return plugin;
}
