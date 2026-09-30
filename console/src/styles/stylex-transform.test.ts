import { runInNewContext } from "node:vm";
import * as stylex from "@stylexjs/stylex";
import { expect, it } from "vitest";
import { stylexPlugin } from "../../tooling/stylex";

it("keeps animation CSS and lets icon padding override shared button padding", async () => {
	const plugin = stylexPlugin({ development: true });
	if (typeof plugin.transform !== "function") {
		throw new Error("StyleX transform is missing");
	}
	const source = `
import * as stylex from "@stylexjs/stylex";
const spin = stylex.keyframes({ to: { transform: "rotate(360deg)" } });
export const styles = stylex.create({
  base: { paddingInline: 14, paddingBlock: 7 },
  icon: { padding: 0, animation: spin + " 1s linear infinite" },
});`;
	const filename = new URL("./button-fixture.ts", import.meta.url).pathname;
	const client = await Reflect.apply(plugin.transform, {}, [
		source,
		filename,
		{ ssr: false },
	]);
	const rules: string[] = [];
	const styles: Record<"base" | "icon", stylex.StyleXStyles> = runInNewContext(
		client.code
			.replace(/^import .*;$/gm, "")
			.replace("export const styles =", "globalThis.styles ="),
		{ stylex, _inject: (rule: { ltr: string }) => rules.push(rule.ltr) },
	);
	expect(rules.join("\n")).toMatch(/animation:[^}]+1s linear infinite/);
	expect(rules.join("\n")).toContain("transform:rotate(360deg)");
	expect(stylex.props(styles.base, styles.icon)).toEqual(
		stylex.props(styles.icon),
	);

	const server = await Reflect.apply(plugin.transform, {}, [
		source,
		filename,
		{ ssr: true },
	]);
	expect(server.code).not.toContain("stylex-inject");
});
