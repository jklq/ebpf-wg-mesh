// Package logpipeline holds the shared log-shipping primitives used by
// agents, builders, and the control plane: the documented size limit,
// per-producer rate limiting, the bounded disk-backed spool, read
// cursors, and retry backoff.
//
// Pipeline contract:
//
//   - Log types are runtime, build, deploy, http, and network. The
//     pipeline preserves all five and never invents new ones.
//   - Lines at or under MaxLogLineBytes are stored byte-exact. Longer
//     lines are truncated with truncated=true. The pipeline never
//     parses customer output; structured attributes and event names
//     travel only on lines the platform itself emitted.
//   - Ordering is (observed_at, line_id). line_id is a stable
//     producer-assigned identity, so retried batches collapse into the
//     same rows instead of duplicating.
//   - Delivery is at-least-once with bounded resources. Every shed
//     point (rate limiting, spool overflow, ingest overflow) counts
//     dropped lines per allocation or build, and reads report those
//     windows as explicit gaps instead of silently closing them.
package logpipeline
