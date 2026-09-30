import { defineConfig } from "vitest/config";
import { stylexPlugin } from "./tooling/stylex";

export default defineConfig({
	resolve: { tsconfigPaths: true },
	plugins: [stylexPlugin({ test: true })],
	test: {
		include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
		exclude: ["src/**/*.integration.test.ts", "src/**/*.integration.test.tsx"],
	},
});
