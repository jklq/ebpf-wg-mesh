import type {
	DashboardDomainBinding,
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

// The user-visible readiness of one public hostname, in the order a new domain passes through.
export type DomainBindingPhase =
	| "awaiting-dns"
	| "issuing-certificate"
	| "certificate-failed"
	| "live";

export function domainBindingPhase(
	binding: DashboardDomainBinding,
): DomainBindingPhase {
	if (
		!binding.platformGenerated &&
		binding.ownershipState !== "DOMAIN_OWNERSHIP_STATE_VERIFIED"
	) {
		return "awaiting-dns";
	}
	switch (binding.certificate?.state) {
		case "DOMAIN_CERTIFICATE_STATE_PENDING":
			return "issuing-certificate";
		case "DOMAIN_CERTIFICATE_STATE_FAILED":
			return "certificate-failed";
		default:
			return "live";
	}
}

export function servesHTTPS(binding: DashboardDomainBinding): boolean {
	return binding.certificate?.state === "DOMAIN_CERTIFICATE_STATE_ACTIVE";
}

export function formatCertificateMessage(
	binding: DashboardDomainBinding,
): string | undefined {
	const certificate = binding.certificate;
	if (!certificate?.message) {
		return undefined;
	}
	const retry = certificate.retryAt ? new Date(certificate.retryAt) : null;
	const retryText =
		retry && !Number.isNaN(retry.getTime())
			? ` Retrying at ${retry.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}.`
			: "";
	return `${certificate.message.replace(/\.?$/, ".")}${retryText}`;
}

export function formatOwnershipMessage(
	message: string | undefined,
	platformHostname: string | undefined,
): string {
	const expected = platformHostname
		? `Point a CNAME at ${platformHostname}.`
		: "Point a CNAME at the generated platform hostname.";
	if (!message) {
		return expected;
	}
	if (message.includes("no such host") || message.includes("lookup ")) {
		return `DNS does not resolve yet. ${expected}`;
	}
	const mismatch = /expected ([^,]+), got (.+)$/.exec(message);
	if (mismatch) {
		return `CNAME currently points to ${mismatch[2]}. Expected ${mismatch[1]}.`;
	}
	return message;
}
export function recommendedTargetPort(
	service: DashboardServiceRecord,
	status: DashboardServiceStatus | null,
): number {
	const primary = service.spec?.runtime?.ports.find((port) => port.primary);
	if (primary?.port) {
		return primary.port;
	}
	const healthy = [
		...(status?.allocation?.healthyIpv4Ports ?? []),
		...(status?.allocation?.healthyIpv6Ports ?? []),
	].find((port) => Number.isInteger(port) && port >= 1 && port <= 65535);
	return healthy ?? 8080;
}
