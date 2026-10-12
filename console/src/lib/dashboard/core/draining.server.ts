import { readFileSync } from "node:fs";

// Host administration writes the systemd invocation it is stopping to this file
// before SIGTERM, so balancers withdraw the replica while it still serves. A
// restarted process has a new invocation and never inherits the marker.
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
