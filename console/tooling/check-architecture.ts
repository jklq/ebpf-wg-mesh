import { readdir, readFile } from "node:fs/promises";
import { dirname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const root = fileURLToPath(new URL("../", import.meta.url));
const sourceRoot = resolve(root, "src");
const violations: string[] = [];

async function sourceFiles(directory: string): Promise<string[]> {
	const entries = await readdir(directory, { withFileTypes: true });
	const groups = await Promise.all(
		entries.map((entry) => {
			const path = resolve(directory, entry.name);
			if (entry.isDirectory()) return sourceFiles(path);
			return [path];
		}),
	);
	return groups.flat();
}

function isProductionSource(path: string) {
	return (
		/\.tsx?$/.test(path) &&
		!path.includes("/platform-gen/") &&
		!path.endsWith("/routeTree.gen.ts") &&
		!/\.(?:integration\.)?test\.tsx?$/.test(path) &&
		!path.endsWith(".test-helpers.ts")
	);
}

for (const path of (await sourceFiles(sourceRoot)).sort()) {
	const file = relative(sourceRoot, path);
	if (file.endsWith(".css") && file !== "styles/reset.css") {
		violations.push(`src/${file}: author component styles with StyleX.`);
	}
	if (!isProductionSource(path)) continue;
	const text = await readFile(path, "utf8");
	const source = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true);
	const report = (node: ts.Node, message: string) => {
		const { line } = source.getLineAndCharacterOfPosition(
			node.getStart(source),
		);
		violations.push(`src/${file}:${line + 1}: ${message}`);
	};
	const checkImport = (specifier: ts.StringLiteralLike) => {
		const name = specifier.text;
		if (/tailwind/.test(name)) report(specifier, "Tailwind has been removed.");
		const importedPath = name.startsWith("#/")
			? resolve(sourceRoot, name.slice(2))
			: name.startsWith(".")
				? resolve(dirname(path), name)
				: undefined;
		if (!importedPath) return;
		const target = relative(sourceRoot, importedPath);
		if (
			/^(features|components|hooks|styles|lib)\//.test(file) &&
			target.startsWith("routes/")
		)
			report(
				specifier,
				"Routes are adapters; implementation modules cannot import them.",
			);
		if (
			/^(components|hooks|styles|lib)\//.test(file) &&
			target.startsWith("features/")
		)
			report(specifier, "Shared modules cannot depend on features.");
		if (
			/^(components\/(ui|layout)|styles)\//.test(file) &&
			/^(lib|hooks)\//.test(target)
		)
			report(
				specifier,
				"UI primitives and styles must stay independent of application data.",
			);
	};
	const visit = (node: ts.Node) => {
		if (
			(ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) &&
			node.moduleSpecifier &&
			ts.isStringLiteralLike(node.moduleSpecifier)
		)
			checkImport(node.moduleSpecifier);
		if (
			ts.isCallExpression(node) &&
			node.expression.kind === ts.SyntaxKind.ImportKeyword &&
			node.arguments[0] &&
			ts.isStringLiteralLike(node.arguments[0])
		)
			checkImport(node.arguments[0]);
		if (
			ts.isImportTypeNode(node) &&
			ts.isLiteralTypeNode(node.argument) &&
			ts.isStringLiteralLike(node.argument.literal)
		) {
			checkImport(node.argument.literal);
		}
		if (
			ts.isJsxAttribute(node) &&
			["className", "style"].includes(node.name.getText(source))
		) {
			report(
				node,
				"Apply compiled styles with stylex.props; accept StyleXStyles for composition.",
			);
		}
		if (
			ts.isCallExpression(node) &&
			node.expression.getText(source) === "stylex.defineVars" &&
			!file.endsWith(".stylex.ts")
		) {
			report(node, "Shared variables must live in a .stylex.ts module.");
		}
		if (
			ts.isCallExpression(node) &&
			node.expression.getText(source) === "stylex.create"
		) {
			const styles = node.arguments[0];
			if (styles && ts.isObjectLiteralExpression(styles)) {
				for (const style of styles.properties) {
					const name = style.name?.getText(source).replace(/["']/g, "");
					if (
						name &&
						/^(div|span|button|input|label|section|p|a|form|h[1-6])(?:\d+|Default\w*)$/.test(
							name,
						)
					) {
						report(
							style,
							`Name "${name}" for its purpose or state, such as toolbar or selected.`,
						);
					}
				}
			}
		}
		ts.forEachChild(node, visit);
	};
	visit(source);
}

const manifest = JSON.parse(
	await readFile(resolve(root, "package.json"), "utf8"),
);
for (const name of Object.keys({
	...manifest.dependencies,
	...manifest.devDependencies,
})) {
	if (/tailwind/.test(name))
		violations.push(`package.json: remove ${name}; styles use StyleX.`);
}

if (violations.length) {
	console.error(violations.join("\n"));
	process.exitCode = 1;
} else {
	console.log(
		"Architecture checks passed: dependency direction and StyleX authoring.",
	);
}
