import { readFileSync } from "node:fs";

// The marker names a systemd invocation so a restarted process cannot inherit it.
export function isDraining(
	env: NodeJS.ProcessEnv = process.env,
	read: (file: string) => string = (file) => readFileSync(file, "utf8"),
): boolean {
	const file = env.DASHBOARD_DRAIN_FILE?.trim();
	const invocation = env.INVOCATION_ID?.trim();
	if (!file || !invocation) return false;
	try {
		return read(file).trim() === invocation;
	} catch {
		return false;
	}
}
