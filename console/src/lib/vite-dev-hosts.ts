export function buildAllowedDevHosts(
	publicBaseURL?: string,
	ingressTargetHost?: string,
): string[] {
	const hosts = new Set<string>(["127.0.0.1", "localhost"]);
	for (const value of [publicBaseURL, ingressTargetHost]) {
		const trimmed = value?.trim();
		if (!trimmed) {
			continue;
		}
		try {
			const parsed = new URL(trimmed);
			if (parsed.hostname) {
				hosts.add(parsed.hostname);
			}
		} catch {
			if (!trimmed.includes("://") && !trimmed.includes("/")) {
				hosts.add(trimmed);
			}
		}
	}
	return [...hosts];
}
