# Storage operations

The agent defaults to `AGENT_SNAPSHOTTER=auto`. It selects overlayfs when the
containerd plugin is ready. If overlayfs is unavailable, it selects native.
Set `AGENT_SNAPSHOTTER` or `--snapshotter` to select a specific plugin.

Overlayfs shares image layers between replicas. Each replica has a separate
writable layer. The agent measures snapshot disk usage at most once per
allocation every 30 seconds. Disk enforcement can lag writes by this interval,
plus the time until the next reconciliation. This check does not impose a
filesystem quota.

The control plane keeps current intent, unfinished deployment work, and rows
that allocations reference in its resident journal projection. Older revisions,
rollouts, and deployments remain in PostgreSQL. Deployment history and rollback
requests use those SQL records. Journal compaction does not delete product history.

## Registry collection

Image retention deletes expired manifests through the registry API. The
production operations service also collects unreferenced blobs. The daily timer
runs at 03:00 host time, with up to 15 minutes of random delay. Failed operations
retry every minute. `alarmWebhookUrl` receives collection failures when configured.

The collector starts after the first build artifact and requires a finalized
installation plan. It holds the maintenance leases for credentials and backups.
It drains builders and waits for active
builds to finish. It then stops all registry replicas and installs systemd gates
that prevent their restart during collection. It runs the deployed Distribution
binary once against the shared storage configuration.

Registry pulls and uploads are unavailable during collection. After collection,
the service confirms that all collector processes stopped. It restarts the
registry replicas and restores each builder's previous drain intent. If the
service cannot confirm collector shutdown, the upload gates remain in place.
The next attempt repeats the operation with the same plan.

Run the supported operation manually with the generated schedule inputs:

```sh
operations --plan /path/to/schedule/plan.json \
  --config /path/to/schedule/operations.json registry-gc
```

Use the `plan.json` and `operations.json` paths from the installed
`platform-<installation>-recovery-registry-gc.service` unit. Inspect that service
on a completion host after a failure. Do not remove a registry gate while a
collector process can still access the storage.

The storage measurements in [retention.md](benchmarks/retention.md) show why
manifest deletion and blob collection are separate operations.
