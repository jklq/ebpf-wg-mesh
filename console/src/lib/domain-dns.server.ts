import { promises as dns } from "node:dns";
import { isIP } from "node:net";
import { URL } from "node:url";

export type DomainVerificationState =
  | "not_found"
  | "wrong_target"
  | "pending_propagation"
  | "verified";

export interface DomainVerificationResult {
  hostname: string;
  isApex: boolean;
  instruction: string;
  state: DomainVerificationState;
  observedCnameChain: Array<string>;
  observedAddresses: Array<string>;
}

export interface DNSResolver {
  resolveCname(hostname: string): Promise<Array<string>>;
  resolve4(hostname: string): Promise<Array<string>>;
  resolve6(hostname: string): Promise<Array<string>>;
}

export const nodeDNSResolver: DNSResolver = {
  resolveCname(hostname) {
    return dns.resolveCname(hostname);
  },
  resolve4(hostname) {
    return dns.resolve4(hostname);
  },
  resolve6(hostname) {
    return dns.resolve6(hostname);
  },
};

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

export async function verifyHostnameDNS(
  hostname: string,
  ingressTargetHost: string,
  resolver: DNSResolver = nodeDNSResolver,
  localDomainSuffix?: string,
): Promise<DomainVerificationResult> {
  const normalizedHostname = normalizeHostname(hostname);
  const normalizedIngressHost = normalizeHostname(ingressTargetHost);
  const normalizedLocalSuffix = normalizeLocalDomainSuffix(localDomainSuffix);
  if (
    normalizedLocalSuffix &&
    isReservedLocalHostname(normalizedHostname, normalizedLocalSuffix)
  ) {
    return {
      hostname: normalizedHostname,
      isApex: false,
      instruction: `Local hostname accepted under .${normalizedLocalSuffix} for localteststack ingress.`,
      state: "verified",
      observedCnameChain: [],
      observedAddresses: [],
    };
  }
  const isApex = isLikelyApexHostname(normalizedHostname);
  const instruction = isApex
    ? `Use ALIAS/ANAME/CNAME flattening from ${normalizedHostname} to ${normalizedIngressHost}.`
    : `Create a CNAME from ${normalizedHostname} to ${normalizedIngressHost}.`;

  if (!isApex) {
    const chain = await resolveCnameChain(normalizedHostname, resolver);
    if (chain.kind === "not_found") {
      return {
        hostname: normalizedHostname,
        isApex,
        instruction,
        state: "not_found",
        observedCnameChain: [],
        observedAddresses: [],
      };
    }
    const observed = chain.chain.map((entry) =>
      entry.replace(/\.+$/, "").toLowerCase(),
    );
    const last = observed.at(-1) ?? "";
    return {
      hostname: normalizedHostname,
      isApex,
      instruction,
      state:
        last === normalizedIngressHost
          ? "verified"
          : observed.length > 0
            ? "wrong_target"
            : "pending_propagation",
      observedCnameChain: observed,
      observedAddresses: [],
    };
  }

  const [hostIPv4, hostIPv6, targetIPv4, targetIPv6] = await Promise.all([
    resolveAddresses(normalizedHostname, resolver.resolve4),
    resolveAddresses(normalizedHostname, resolver.resolve6),
    resolveAddresses(normalizedIngressHost, resolver.resolve4),
    resolveAddresses(normalizedIngressHost, resolver.resolve6),
  ]);
  const observedAddresses = [...hostIPv4, ...hostIPv6].sort();
  const expectedAddresses = new Set([...targetIPv4, ...targetIPv6]);
  if (observedAddresses.length === 0) {
    return {
      hostname: normalizedHostname,
      isApex,
      instruction,
      state: "not_found",
      observedCnameChain: [],
      observedAddresses,
    };
  }
  if (expectedAddresses.size === 0) {
    return {
      hostname: normalizedHostname,
      isApex,
      instruction,
      state: "pending_propagation",
      observedCnameChain: [],
      observedAddresses,
    };
  }
  const matches = observedAddresses.every((address) =>
    expectedAddresses.has(address),
  );
  return {
    hostname: normalizedHostname,
    isApex,
    instruction,
    state: matches ? "verified" : "wrong_target",
    observedCnameChain: [],
    observedAddresses,
  };
}

async function resolveCnameChain(
  hostname: string,
  resolver: DNSResolver,
  maxDepth = 8,
): Promise<
  | { kind: "ok"; chain: Array<string> }
  | { kind: "not_found" }
  | { kind: "wrong_target"; chain: Array<string> }
> {
  const chain: Array<string> = [];
  let current = hostname;
  for (let depth = 0; depth < maxDepth; depth += 1) {
    try {
      const values = await resolver.resolveCname(current);
      const next = values[0]?.trim();
      if (!next) {
        return { kind: "wrong_target", chain };
      }
      chain.push(next);
      current = next.replace(/\.+$/, "");
    } catch (error) {
      if (isNotFoundError(error)) {
        return chain.length === 0
          ? { kind: "not_found" }
          : { kind: "ok", chain };
      }
      throw error;
    }
  }
  return { kind: "wrong_target", chain };
}

async function resolveAddresses(
  hostname: string,
  run: (hostname: string) => Promise<Array<string>>,
): Promise<Array<string>> {
  try {
    return (await run(hostname)).map((entry) => entry.trim()).filter(Boolean);
  } catch (error) {
    if (isNotFoundError(error)) {
      return [];
    }
    throw error;
  }
}

function isNotFoundError(error: unknown): boolean {
  return Boolean(
    error &&
    typeof error === "object" &&
    "code" in error &&
    ["ENOTFOUND", "ENODATA", "SERVFAIL", "ESERVFAIL"].includes(
      String((error as { code?: unknown }).code),
    ),
  );
}

function countLabels(hostname: string): number {
  return hostname.split(".").filter(Boolean).length;
}

function isLikelyApexHostname(hostname: string): boolean {
  const labels = countLabels(hostname);
  if (labels <= 2) {
    return true;
  }
  const apexWithKnownPublicSuffix = [
    "co.uk",
    "org.uk",
    "gov.uk",
    "ac.uk",
    "com.au",
    "net.au",
    "org.au",
    "co.nz",
    "co.jp",
    "com.br",
    "com.mx",
    "com.sg",
  ].some((suffix) => hostname.endsWith(`.${suffix}`));
  return apexWithKnownPublicSuffix && labels === 3;
}

function isIPAddress(value: string): boolean {
  return isIP(value) !== 0;
}

function normalizeLocalDomainSuffix(value?: string): string | undefined {
  const trimmed = value
    ?.trim()
    .toLowerCase()
    .replace(/^\.+/, "")
    .replace(/\.+$/, "");
  return trimmed || undefined;
}

function isReservedLocalHostname(hostname: string, suffix: string): boolean {
  return hostname === suffix || hostname.endsWith(`.${suffix}`);
}
