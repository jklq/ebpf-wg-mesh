# Resource usage

Resource usage tracking measures what allocations and volumes actually
consume, in resource units. There are no prices in this system.

## Meters (`usage/v1`)

| Meter | Measurement | Stored quantity | Collection |
| --- | --- | --- | --- |
| CPU | Delta of allocation cgroup `cpu.stat usage_usec` | CPU microseconds | Every 5 seconds and at lifecycle edges |
| Memory | Integral of allocation cgroup `memory.current` | Byte-milliseconds | Every 5 seconds and at lifecycle edges |
| Volume | Integral of used filesystem blocks | Byte-milliseconds | Every 60 seconds and at lifecycle edges |
| Public egress | Allowed outbound logical IP bytes at the host-side veth | Bytes | Kernel counter, read every 5 seconds |

Memory is total cgroup memory, including attributable page cache and kernel
memory. The console shows this definition beside the memory values.

Gauges (memory, volume) hold the previous valid sample for at most two sample
periods; time beyond that is uncovered and the record carries `GAUGE_GAP`.
CPU and egress are cumulative counters: a surviving counter recovers its
delta after a collector restart, flagged `UNCERTAIN_TIMING` when the interval
exceeds two sample periods.

Each task incarnation is a counter epoch, `"<boot_id>/<cgroup_id>"`. The
cgroup ID is the cgroup directory inode, never reused within a boot; it is
also the egress counter token. Restarts recreate the container, cgroup, and
veth, so they start a new epoch. Counters are never subtracted across epochs.

## Records and buckets

The agent seals one immutable record per resource per minute at the first
sample at or after each UTC minute boundary. Partial first and last minutes
keep their true durations; runtime is never rounded to a minute.

Queries assign each interval to the bucket containing its start and never
split intervals. `GetProjectUsage` resolutions: MINUTE up to 24 hours from
raw intervals, HOUR up to 31 days and DAY up to 366 days from hourly
aggregates. Averages divide by the window up to the watermark; gauge averages
scale each resource's covered integral to its measured time.

## Delivery

Each agent keeps a bbolt usage journal under `<data-dir>/usage/`. Records are
numbered per producer (one journal identity) and kept until the control
plane's `UsageAcknowledgement` confirms durable acceptance in PostgreSQL.
Acknowledged records stay resendable for 24 hours (configurable) so a
database restore can replay them; records pruned past a restored cursor are
reported once as `lost_through` and stored as explicit producer gaps.

Unacknowledged records are never evicted. When the journal reaches its cap
(default 256 MiB, `--usage-journal-max-bytes`), collection pauses with
checkpoints frozen; recovery behaves like a collector restart and lost final
counters become gap records. Agent journals are not backed up: losing an
agent's disk loses its unacknowledged records, and the replacement journal is
recorded as a producer gap.

## Egress counting

Counting happens once, in the TC ingress hook on the host-side veth, which
sees packets leaving the allocation, and only on allowed paths: after source
validation, after conntrack recording, on the successful peer-redirect path,
and on the allowed ICMP path. Denied traffic, ARP, and NDP are not counted.

Bytes are logical IP bytes: frame length minus Ethernet, with GSO
superframes normalized by adding per-segment L3+L4 headers. Retransmissions
seen at the interface count. This is application output before Envoy TLS or
compression; it is not ISP wire bytes.

Classes: `public_direct`, `public_ingress` (responses to trusted ingress
prefixes, which may also open TCP connections to allocations), `private`
(same network identity), `platform` (platform prefixes), and `probe`.
Ingress health checks that never carry payload in either direction classify
as probe; control-only bytes are held as pending per flow, and evicting a
flow loses its pending bytes (a bounded undercount). Only public direct plus
public ingress are public egress.

This changes the datapath: without ingress-prefix admission, Envoy could not
reach allocations at all, because inbound connections previously required a
mesh identity or a reverse conntrack entry.

## Retention and freshness

Raw intervals are kept 30 days (`--usage-raw-retention-days`), hourly
aggregates 90 days (`--usage-hourly-retention-days`). Producer cursors that
prevent replay double-counting are kept indefinitely.

The query watermark is the oldest `sealed_through` across producers of agents
with current resources. Results are stale when a current producer is missing
or has not sealed for over 90 seconds. Buckets ending after the watermark are
incomplete; missing data is shown as missing, never as zero.

## Alarms

The live owner alarms once per condition while it holds: journal above 80%
or exhausted, new gap or producer-gap records, a stale producer of an active
agent, a blocked (conflicting) producer, recent rejections, and growing
unattributed egress.

## Limitations

- Memory and volume are sampled integrals, not exact integrals.
- Final counters of a task that vanishes between samples are lost; the
  remainder is an explicit gap.
- A totally lost agent disk destroys unreported usage. No usage is invented.
- Egress pending bytes of evicted conntrack flows are undercounted.
