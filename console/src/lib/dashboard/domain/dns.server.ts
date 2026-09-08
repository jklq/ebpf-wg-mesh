import { isIP } from "node:net";
import { URL } from "node:url";

export function normalizeHostname(raw: string): string {
	const value = raw.trim().toLowerCase().replace(/\.+$/, "");
	if (!value) {
		throw new Error("Hostname is required.");
	}
	if (value.includes("://")) {
		throw new Error("Enter only a hostname, without a URL scheme.");
	}
	if (value.includes("/") || value.includes("?") || value.includes("#")) {
		throw new Error("Enter only a hostname, without a path or query.");
	}
	if (value.includes("*")) {
		throw new Error("Wildcards are not supported.");
	}
	if (value === "localhost" || !value.includes(".")) {
		throw new Error("Enter a fully qualified hostname.");
	}
	if (isIPAddress(value)) {
		throw new Error("IP addresses are not valid hostnames here.");
	}
	if (value.includes(":")) {
		try {
			const parsed = new URL(`https://${value}`);
			if (parsed.hostname !== value) {
				throw new Error("Enter only a hostname, without a port.");
			}
		} catch {
			throw new Error("Enter only a hostname, without a port.");
		}
	}
	return value;
}

function isIPAddress(value: string): boolean {
	return isIP(value) !== 0;
}
