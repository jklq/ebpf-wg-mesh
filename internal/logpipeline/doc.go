// Package logpipeline holds the shared log-shipping primitives used by agents,
// builders, and the control plane: the size limit, rate limiting, the
// disk-backed spool, read cursors, and retry backoff.
//
// Pipeline contract:
//
//   - Log types are runtime, build, deploy, http, and network. The
//     pipeline preserves all five and never invents new ones.
//   - Lines at or under MaxLogLineBytes are stored byte-exact; longer
//     lines truncate with truncated=true. The pipeline never parses
//     customer output; attributes travel only on platform-emitted lines.
//   - Ordering is (observed_at, line_id). line_id is a stable
//     producer-assigned identity, so retries collapse instead of duplicating.
//   - Delivery is at-least-once with bounded resources. Shed points
//     (rate limiting, spool overflow, ingest overflow) count drops per
//     allocation or build, and reads report them as explicit gaps.
package logpipeline
