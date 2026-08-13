import {
	type DashboardConfig,
	DashboardConfigError,
	type DevLoginIdentity,
} from "#/lib/dashboard/core/types.server";

export function sanitizeRedirect(value?: string): string {
	if (
		!value ||
		!value.startsWith("/") ||
		value.startsWith("//") ||
		value.includes("\\")
	) {
		return "/";
	}
	return value;
}

export function formatError(error: unknown): string {
	if (error && typeof error === "object" && "message" in error) {
		return String(error.message);
	}
	return "unknown error";
}

export function parseIdentifier(raw: string): string {
	if (/^[A-Za-z_][A-Za-z0-9_]*$/.test(raw)) {
		return raw;
	}
	throw new DashboardConfigError({
		message: `invalid dashboard schema identifier: ${raw}`,
	});
}

export function parseDevUsers(raw: string): Array<DevLoginIdentity> {
	if (raw.trim() === "") {
		return [];
	}
	return raw
		.split(";")
		.map((entry) => entry.trim())
		.filter((entry) => entry !== "")
		.flatMap((entry) => {
			const [id, email] = entry.split(":", 2);
			const parsed = {
				id: (id ?? "").trim(),
				email: (email ?? "").trim(),
			};
			if (parsed.id === "" || parsed.email === "") {
				return [];
			}
			return [parsed];
		});
}

export function shouldUseSecureCookies(config: DashboardConfig): boolean {
	if (config.profile === "production") {
		return true;
	}
	return (
		config.publicBaseURL.startsWith("https://") &&
		!config.localDomainSuffix?.trim()
	);
}
