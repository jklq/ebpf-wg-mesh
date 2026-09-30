import type {
	DashboardServiceRecord,
	DashboardServiceStatus,
} from "#/lib/dashboard/core/types.server";

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
