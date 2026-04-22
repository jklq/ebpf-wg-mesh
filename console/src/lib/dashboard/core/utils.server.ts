import {
	type DashboardConfig,
	DashboardConfigError,
	type DevLoginIdentity,
} from "#/lib/dashboard/core/types.server";

export function sanitizeRedirect(value?: string): string {
	if (!value || !value.startsWith("/")) {
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
			const [subject, email] = entry.split(":", 2);
			const parsed = {
				subject: (subject ?? "").trim(),
				email: (email ?? "").trim(),
			};
			if (parsed.subject === "" || parsed.email === "") {
				return [];
			}
			return [parsed];
		});
}

export function shouldUseSecureCookies(config: DashboardConfig): boolean {
	return (
		config.publicBaseURL.startsWith("https://") &&
		!config.localDomainSuffix?.trim()
	);
}
