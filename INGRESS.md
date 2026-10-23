# Ingress deployment

The xDS listener always requires TLS 1.3 and a client certificate issued by the
platform internal CA. Only the `ingress` caller class can retrieve configuration,
including SDS private keys. Fleet agent, builder, dashboard, and workload
credentials cannot access it. Each ingress instance needs its own stable node ID;
the certificate common name and Envoy's `node.id` must match exactly. Reuse that
ID across process restarts and endpoint changes. Do not share ingress credentials
with workloads or grant ingress hosts access to CA signing keys.

Configure `--internal-server-names` with the DNS name every xDS replica presents,
such as `controlplane.internal.example.net`. Both internal RPCs and xDS use that
rotating server identity. Set `--ingress-xds-listen` to the reachable address.
Restrict the listener to ingress networks as an additional boundary.

## Provisioning an ingress instance

On an operator-controlled control plane host, with its database connection and
envelope key provider configured, issue a dedicated identity:

```sh
controlplane signing-keys issue-client-cert \
  --db-url "$CONTROLPLANE_DATABASE_URL" --state-dir /var/lib/controlplane \
  --caller-class ingress --caller-id ingress-eu-1 --ttl 24h \
  --out-dir /srv/ingress-eu-1/identity --mounted-dir /etc/envoy/identity

controlplane ingress-bootstrap \
  --node-id ingress-eu-1 --identity-dir /etc/envoy/identity \
  --server-name controlplane.internal.example.net \
  --xds-addresses cp1.internal.example.net:18000,cp2.internal.example.net:18000 \
  > /srv/ingress-eu-1/envoy.yaml
```

Use the same secret-key provider flags as the control plane for non-file
providers. Securely deploy the bootstrap and identity directory to that ingress
host. Mount the entire identity directory at `/etc/envoy/identity` read-only;
mounting individual files hides rotation events. Allow only the Envoy user and
the credential delivery process to read it. Private keys are mode `0600` and
credential generations are mode `0700`; preserve these permissions and set the
owner to the Envoy user during delivery. Keep the admin listener on loopback.

The bootstrap uses local filesystem SDS for client credentials and CA trust,
verifies the server SAN, and enables ADS for dynamic listeners and clusters.
Delta xDS is unsupported and rejected. There is no plaintext or development
authentication bypass.

## Rotation

Ingress leaves have a maximum 24-hour lifetime. Renewal is automatic; no
person runs it on a schedule. On a platformctl-managed installation, the
credentials timer renews every leaf hourly and pages the alarm webhook when
a round fails. On an installation managed by hand, install the renewal timer
once (see `infra/production/ingress/`): it runs the provisioning command
every six hours, and `--alarm-webhook-url` (or `CONTROLPLANE_ALARM_WEBHOOK_URL`)
makes a failed run page the operator. Set the alarm destination in exactly
one place per surface — `alarmWebhookUrl` in the operations configuration
for managed installations, `--alarm-webhook-url` on the control plane and
renewal commands otherwise. There is no default destination; with none set,
failures stay in the logs. Deploy the complete new generation before expiry.
The command validates the key pair and atomically switches the
`current` symlink. Envoy watches the identity directory through filesystem SDS
and reloads the certificate, key, and trust bundle without a restart (see
[Envoy filesystem SDS rotation](https://www.envoyproxy.io/docs/envoy/v1.36.0/configuration/security/secret)). For remote
delivery, transfer the complete generation first, then atomically replace that
host's `current` symlink; never update the active certificate and key in place.
Retain the previous generation until Envoy reports successful SDS reload, then
prune older unreferenced generations. Production renewal never restarts Envoy for
an identity change. It checks Envoy's admin `/certs` until the running process
lists the installed certificate's serial number.

For CA rotation, run `signing-keys rotate-start --scope internal-ca`, provision
the overlap bundle to every ingress and internal client, and renew their leaves.
The xDS server reloads its trust bundle on every handshake and rotates its own
leaf in the existing authority refresh loop. Finish the rotation only after the
configured CA overlap and client renewal; then deploy the final bundle. Both old
and new CA leaves are accepted during overlap. Revoked and expired leaves are
rechecked on each request and configuration push, including existing streams.

Identity expiry needs no separate expiry monitor: the renewal alarm fires
while hours of margin remain. Still monitor Envoy's SDS reload failures and
xDS ACK/NACK state.
The VM probe and local stack use the same authenticated transport and stable
node IDs.

The xDS server bounds connection age to one hour (with gRPC's age jitter
and a one-minute grace). The Envoy xDS cluster uses one ADS request per
connection, so a closed stream reconnects with the newly loaded credentials
instead of reusing a revoked HTTP/2 connection.

## Load-balancer health and draining

Envoy closes its listeners immediately on shutdown, so a drain must begin before
the process is signalled (see
[Envoy draining](https://www.envoyproxy.io/docs/envoy/v1.36.0/intro/arch_overview/operations/draining)).
With `--health-address`, the bootstrap adds a static health listener that answers
`GET /healthz` with the HTTP health check filter. Envoy opens this listener only
after it is live with its initial configuration. Configure the load balancer to
check it. Production uses `ingressHealthListen`, which defaults to
`0.0.0.0:19001`.

The production unit runs `operations prepare-stop envoy` as `ExecStop=` before
every stop. It posts `/healthcheck/fail`, so the health listener returns 503 and
each response closes its connection. It waits until no new ingress connections
arrive. It then posts `/drain_listeners?graceful&skip_exit` and waits for active
downstream connections to finish before systemd sends SIGTERM. Run Envoy with
`--drain-strategy immediate` and a `--drain-time-s` shorter than that wait.

Withdrawal waits apply when the installed role has peers and this Envoy has
received health checks. With no observed checks, stop preparation proceeds
directly to the listener drain. A sole Envoy keeps serving until SIGTERM and
waits only for active requests, capped at ten seconds. This avoids a withdrawal
outage with nowhere else to send traffic. Drain waits remain bounded; long-lived
connections that outlast them can be closed at shutdown. Replicated ingress
still needs a correctly configured balancer and enough surviving capacity.

## Membership and retirement

Provisioning records the node as active before releasing credentials. Every
active node participates in certificate challenge and allocation withdrawal
barriers, even before its first connection. Reissuing credentials for the same
active ID preserves its observations. A process restart or temporary network
failure never retires a node: a disconnected Envoy may still route traffic with
its previous configuration, so it must continue to block a newer snapshot until
it ACKs it.

For permanent removal, remove the instance from the load balancer or DNS traffic
pool, wait for that removal to propagate and existing downstream requests to
finish, and stop Envoy. In production the stop preparation performs the
health-check withdrawal and drain. Static DNS pools still need explicit removal.
Only then retire it:

```sh
controlplane ingress-nodes list --db-url "$CONTROLPLANE_DATABASE_URL"
controlplane ingress-nodes retire --db-url "$CONTROLPLANE_DATABASE_URL" \
  --node-id ingress-eu-1 --traffic-stopped
```

`--traffic-stopped` is the operator's assertion that removal and shutdown have
completed; the control plane cannot inspect an external load balancer. Retirement
immediately removes the node from both barriers and denies its existing and
future xDS requests and pushes. The tombstone remains permanently. Delayed
observations and credential provisioning cannot reactivate it. A replacement
instance needs a new ID. Do not retire an instance just to clear a temporary
failure or NACK.

Ingress membership uses schema version 38. As with other schema cutovers in this
repository, older schemas are rejected; there is no incremental migration.

## Shared wildcard for generated hostnames

For `--ingress-public-addr apps.example.net`, production requires a currently
valid certificate with the exact SAN `*.apps.example.net` and its matching key.
Configure both `--ingress-platform-tls-cert-file` and
`--ingress-platform-tls-key-file` on every control plane replica. A certificate
for an individual generated name or `probe.apps.example.net` is insufficient.
Generated hostnames exclusively use this wildcard; creating services or domain
bindings never creates individual ACME orders for those names. The wildcard
covers one DNS label beneath the suffix; the suffix apex needs its own
certificate if routed as an operator-configured host.

Provision and renew the wildcard externally using DNS-01. HTTP-01 cannot issue
wildcards; DNS-01 can also delegate `_acme-challenge.apps.example.net` to a
separate validation zone. See [Let's Encrypt's challenge documentation](https://letsencrypt.org/docs/challenge-types/).
Keep the DNS credential on the renewal host, separate from workloads and the
control plane. Scope its permissions to the necessary DNS zone.

For example, install Certbot and its DNS provider plugin on the renewal host.
For Cloudflare, put a zone-scoped `Zone:DNS:Edit` API token in
`/etc/letsencrypt/dns.ini`, owned by the renewal user and mode `0600`:

```ini
dns_cloudflare_api_token = YOUR_ZONE_SCOPED_TOKEN
```

```sh
certbot certonly --non-interactive --agree-tos --email ops@example.net \
  --dns-cloudflare --dns-cloudflare-credentials /etc/letsencrypt/dns.ini \
  --cert-name platform-apps -d '*.apps.example.net'
```

The plugin's [credential and installation instructions](https://certbot-dns-cloudflare.readthedocs.io/en/stable/)
apply; use the equivalent plugin for another DNS provider. Install only one
renewal job for this shared certificate, rather than one job per replica or
service. Enable your installation's Certbot renewal timer and verify it with
`certbot renew --dry-run`. Use a
[deploy hook](https://eff-certbot.readthedocs.io/en/stable/using.html#renewing-certificates)
to publish `fullchain.pem` and `privkey.pem` after a successful renewal.

Deploy the complete chain and key to a new generation on the shared,
operator-controlled certificate volume. Give the control plane user read access
to that generation and keep the private key mode `0600`. Atomically switch the
`current` symlink after both files arrive. Every replica must mount the same
parent directory read-only and resolve the same generation, for example:

```sh
controlplane --profile production --ingress-public-addr apps.example.net \
  --ingress-platform-tls-cert-file /etc/ebpf-wg-mesh/ingress/current/fullchain.pem \
  --ingress-platform-tls-key-file /etc/ebpf-wg-mesh/ingress/current/privkey.pem \
  ...
```

Supply the rest of the production flags for database, signing keys, storage, and
listeners as usual. Renewal changes are picked up by the existing certificate
file reload and xDS publication loop; Envoy receives the new wildcard through
authenticated SDS without a restart. An incomplete or invalid replacement keeps
the previous valid certificate. Missing, expired, mismatched, or incorrect
wildcard material is rejected at startup. Expiry during operation fails
publication and certificate status rather than removing HTTPS from the last
good snapshot or ordering individual certificates. The wildcard itself stays
externally renewed on purpose — the DNS-01 credential never enters the
platform — but its expiry is now watched by the control plane: with an alarm
webhook set, it pages the operator at 21 days, 7 days, and 24 hours of
remaining lifetime, once per level, and when the deployed files cannot be
loaded at all. A silent external renewal failure therefore becomes an alarm
long before it becomes an outage.

Custom domains retain individual ACME HTTP-01 issuance and renewal, which the
control plane already runs alone before expiry. A failed attempt pages the
alarm webhook only when service is actually at risk: no certificate yet, an
expired certificate, or under 72 hours of lifetime left. Earlier failures
retry alone on the existing backoff. Their CNAME
ownership checks still point at the service's generated hostname. Keep port 80
reachable for those custom-domain challenges; ordinary certified traffic
redirects to HTTPS. The control plane needs no DNS-01 credentials.

## Serverless wake activator

Hostnames of sleeping serverless services (see
[Serverless services (scale-to-zero)](docs/serverless.md)) route to a shared
`activator` cluster instead of workload endpoints. Run one `activator`
binary next to each Envoy; it calls the control plane's internal
`ActivatorService.WakeService`, holds the request, and proxies it to the
first ready endpoint. The control plane publishes the cluster at
`--ingress-activator-upstream` (default `127.0.0.1:18081`), so loopback
reaches the local activator on every ingress host with no per-host
configuration in the snapshot.

Provision a dedicated identity per activator instance — never share the
ingress xDS credentials:

```sh
controlplane signing-keys issue-client-cert \
  --db-url "$CONTROLPLANE_DATABASE_URL" --state-dir /var/lib/controlplane \
  --caller-class activator --caller-id activator-eu-1 --ttl 24h \
  --out-dir /srv/activator-eu-1/identity --mounted-dir /etc/platform-activator/identity
```

Deliver and renew it exactly like the ingress identity (complete generation,
atomic `current` switch, hourly timer); the activator reloads the current
generation without a restart. The `activator` caller class can call only
`ActivatorService`; the control plane resolves the requested hostname to a
service server-side, so a compromised activator cannot name a service it was
not handed traffic for. A template unit is in
`infra/production/ingress/platform-activator.service`. Monitor its
`/healthz` like the Envoy health listener: without a running activator,
requests to sleeping services fail at Envoy (no healthy upstream for the
activator cluster) while awake services are unaffected.
