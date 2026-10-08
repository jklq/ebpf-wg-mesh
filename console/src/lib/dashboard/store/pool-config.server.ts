import { checkServerIdentity } from "node:tls";
import type { PoolConfig } from "pg";
import { parseIntoClientConfig } from "pg-connection-string";

// pg-connection-string preserves URL brackets around IPv6 hosts. Socket APIs
// need a bare address. Verify the selected host explicitly even when it is an
// IP literal, for which pg does not send TLS SNI.
export function poolConfigFromURL(connectionString: string): PoolConfig {
 const cfg = parseIntoClientConfig(connectionString);
 const host = cfg.host?.replace(/^\[(.*)\]$/, "$1");
 if (!host || host.includes(",")) throw new Error("one SQL endpoint required per pool");
 cfg.host = host;
 if (new URL(connectionString).searchParams.get("sslmode") === "verify-full") {
  const tls = typeof cfg.ssl === "object" ? cfg.ssl : {};
  cfg.ssl = {
   ...tls,
   rejectUnauthorized: true,
   checkServerIdentity: (_servername, cert) => checkServerIdentity(host, cert),
  };
 }
 return cfg;
}
