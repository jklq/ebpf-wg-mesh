import { devtools } from "@tanstack/devtools-vite";
import { tanstackStart } from "@tanstack/react-start/plugin/vite";
import viteReact from "@vitejs/plugin-react";
import { nitro } from "nitro/vite";
import { defineConfig } from "vite";
import { buildAllowedDevHosts } from "./src/lib/vite-dev-hosts";
import { stylexPlugin } from "./tooling/stylex";

const config = defineConfig({
	resolve: { tsconfigPaths: true },
	plugins: [
		{
			name: "extend-dev-idle-timeout",
			configureServer(server) {
				server.httpServer?.setTimeout(120_000);
			},
		},
		devtools(),
		stylexPlugin(),
		tanstackStart(),
		nitro(
			process.env.NITRO_PRESET === "bun"
				? ({ preset: "bun" } as never)
				: ({} as never),
		),
		viteReact(),
	],
	server: {
		host: "127.0.0.1",
		hmr: { overlay: false },
		port: 3000,
		strictPort: true,
		allowedHosts: buildAllowedDevHosts(
			process.env.DASHBOARD_PUBLIC_BASE_URL,
			process.env.DASHBOARD_INGRESS_TARGET_HOST,
		),
	},
});

export default config;
