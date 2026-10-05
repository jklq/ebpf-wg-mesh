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

Ingress leaves have a maximum 24-hour lifetime. Run the provisioning command
at least every 12 hours, alert on failure, and deploy the complete new generation
before expiry. The command validates the key pair and atomically switches the
`current` symlink. Envoy watches the identity directory through filesystem SDS
and reloads the certificate, key, and trust bundle without a restart (see
[Envoy filesystem SDS rotation](https://www.envoyproxy.io/docs/envoy/v1.36.0/configuration/security/secret)). For remote
delivery, transfer the complete generation first, then atomically replace that
host's `current` symlink; never update the active certificate and key in place.
Retain the previous generation until Envoy reports successful SDS reload, then
prune older unreferenced generations.

For CA rotation, run `signing-keys rotate-start --scope internal-ca`, provision
the overlap bundle to every ingress and internal client, and renew their leaves.
The xDS server reloads its trust bundle on every handshake and rotates its own
leaf in the existing authority refresh loop. Finish the rotation only after the
configured CA overlap and client renewal; then deploy the final bundle. Both old
and new CA leaves are accepted during overlap. Revoked and expired leaves are
rechecked on each request and configuration push, including existing streams.

Monitor identity expiry, Envoy's SDS reload failures, and xDS ACK/NACK state.
The VM probe and local stack use the same authenticated transport and stable
node IDs.

The xDS server forces a fresh connection at least once per hour (with gRPC's age
jitter and a one-minute grace). The Envoy xDS cluster uses one ADS request per
connection, so a closed stream reconnects with the newly loaded credentials
instead of reusing a revoked HTTP/2 connection.
