import { fileURLToPath } from "node:url";
import stylex from "@stylexjs/unplugin";
import type { Plugin } from "vite";

const rootDir = fileURLToPath(new URL("../", import.meta.url));

// Share compiler settings across client, SSR and tests so token identities match.
export function stylexPlugin({
	test = false,
	development = false,
} = {}): Plugin {
	const options = {
		devMode: "css-only" as const,
		// Variants are applied after base styles, including shorthand resets.
		styleResolution: "application-order" as const,
		useCSSLayers: { before: ["reset"], after: ["accessibility"] },
		runtimeInjection: false,
		aliases: { "#/*": [fileURLToPath(new URL("../src/*", import.meta.url))] },
		unstable_moduleResolution: { type: "commonJS" as const, rootDir },
	};
	const plugin: Plugin = stylex.vite(options);
	// Vitest needs compilation, but no CSS server or HMR polling interval.
	if (test) return { ...plugin, configureServer: undefined };
	if (development && typeof plugin.transform === "function") {
		// SSR extracts the initial sheet. Client modules install their own CSS
		// before rendering, without replacing that sheet during lazy loads or HMR.
		const client = stylex.vite({ ...options, runtimeInjection: true });
		const serverTransform = plugin.transform;
		return {
			...plugin,
			transform(code, id, transformOptions) {
				if (!transformOptions?.ssr && typeof client.transform === "function")
					return client.transform.call(this, code, id, transformOptions);
				return serverTransform.call(this, code, id, transformOptions);
			},
		};
	}
	return plugin;
}
